package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/alessio/shellescape"
	"github.com/bramvdbogaerde/go-scp"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
)

var source string
var releaseId string
var keep int
var hosts []url.URL
var debug bool

func init() {
	flag.Usage = func() {
		_, _ = fmt.Fprintf(os.Stderr, `go-deploy is a tarball deployment tool.

Usage:

  %s [option...] <tarball> <uri...>

Details:

  go-deploy uses SSH, Goroutines and a time tested deployment method to atomically deploy tarballs (the old school way)
  to multiple servers in parallel. You pass in a tarball and a list of URIs, with paths, and the tool will SCP the
  tarball to the server, untar it, and atomically replace a symlink to the newest release. It automatically prunes old
  releases, while holding onto a configurable number of the most recent.  The folder structure created by go-deploy
  looks like this:

	.
	├── current -> ./releases/31e750f5-36ef-4205-9963-df79e1760777
	├── previous -> ./releases/542bcb06-b06f-4719-8ee5-7d32793f6b1b.
	├── releases
	│   ├── 31e750f5-36ef-4205-9963-df79e1760777
	│   │   └── ...
	│   ├── 4d1e2235-2d7a-495c-97ae-d3b88849b79f
	│   │   └── ...
	│   ├── 542bcb06-b06f-4719-8ee5-7d32793f6b1b
	│   │   └── ...
	│   ├── 81892b04-b6fe-4164-85b1-5c192a86e66c
	│   │   └── ...
	│   └── bdf655f6-291f-478d-a56b-648890b57e99
	│       └── ...
	└── shared

  go-deploy is designed to be dead simple, very fast, and quite reliable. It is inspired by Capistrano.

Customization:

  go-deploy can be customized by placing special files in a top-level folder of your releases named ".go-deploy". These
  files can influence the behavior of deploys.

  shared
	A list of files that will be replaced with symlinks to a "shared" folder.

  pre-hook
	A hook that is run immediately before symlinks are swapped.

  post-hook
	A hook that is run immediately after symlinks are swapped.

Options:

`, os.Args[0])
		flag.PrintDefaults()
		_, _ = fmt.Fprintf(os.Stderr, `
Arguments:

  <tarball>
	The path to the tarball you will be deploying.
  <uri...>
	A list of RFC 3986 URIs to deploy to (e.g. ssh://ec2-user@example.com/path/to/release), where "ssh" is the only
	valid protocol.

`)
	}

	flag.StringVar(&releaseId, "id", "<uuid>", "A unique release ID")
	flag.IntVar(&keep, "keep", 5, "The number of releases to keep behind in the releases folder")
	flag.BoolVar(&debug, "debug", false, "Enable debug output")
	flag.Parse()

	if flag.NArg() < 2 {
		flag.Usage()
		os.Exit(1)
	}

	source = flag.Arg(0)

	hosts = []url.URL{}
	for _, arg := range flag.Args()[1:] {
		parse, err := url.Parse(arg)

		if err != nil {
			log.Fatal(err)
		}

		if parse.Scheme != "" && parse.Scheme != "ssh" {
			log.SetOutput(os.Stderr)
			log.Fatalf("unsupported protocol \"%s\" in uri: %s", parse.Scheme, arg)
		}

		if parse.Hostname() == "" {
			log.Fatalf("parse \"%s\": empty hostname (hint: try adding the protocol \"ssh://\")", arg)
		}

		if parse.Path == "" {
			log.Fatalf("parse \"%s\": missing path", arg)
		}

		if parse.Path == "/" {
			log.Fatalf("parse \"%s\": refusing to deploy to root", arg)
		}

		hosts = append(hosts, *parse)
	}

	if releaseId == "<uuid>" {
		releaseId = uuid.NewString()
	}

	if debug {
		log.SetLevel(log.DebugLevel)
	}
}

func main() {
	wg := sync.WaitGroup{}
	wg.Add(len(hosts))

	deployWait := sync.WaitGroup{}
	deployWait.Add(len(hosts))

	for _, host := range hosts {
		host := host
		go func() {
			defer wg.Done()
			err := deployTo(host, &deployWait)
			if err != nil {
				log.WithFields(log.Fields{"host": host.Hostname()}).Fatal(err)
			}
		}()
	}

	wg.Wait()
}

func deployTo(host url.URL, preDeploy *sync.WaitGroup) error {
	client, err := makeClient(host)
	if err != nil {
		return err
	}
	defer func(client *ssh.Client) {
		_ = client.Close()
	}(client)

	releaseRoot := host.Path
	releasePath := path.Join(releaseRoot, "releases", releaseId)
	sharedPath := path.Join(releaseRoot, "shared")
	tmpPath := path.Join(releaseRoot, "tmp")
	tarPath := path.Join(tmpPath, releaseId+".tar")

	logger := log.WithFields(log.Fields{"host": host.Hostname()})

	logger.Info("creating directories")
	err = createFolderHierarchy(client, releaseRoot)
	if err != nil {
		return err
	}

	logger.Info("copying tarball")
	err = copyFile(client, source, tarPath)
	if err != nil {
		return err
	}
	defer func(client *ssh.Client, path string) {
		logger.Info("pruning temp")
		_ = safeRm(client, path)
	}(client, tmpPath)

	logger.Info("extracting tarball")
	err = untarFile(client, tarPath, releasePath)
	if err != nil {
		return err
	}

	logger.Info("replacing shared files")
	err = symlinkSharedFiles(client, releasePath, sharedPath)
	if err != nil {
		return err
	}

	logger.Info("running pre-hook")
	err = runHook(client, releasePath, "pre-hook")
	if err != nil {
		return err
	}

	preDeploy.Done()
	logger.Info("waiting for others..")
	preDeploy.Wait()

	logger.Info("swapping symlinks")
	err = swapSymlinks(client, releaseRoot, releasePath)
	if err != nil {
		return err
	}
	defer func() {
		logger.Info("pruning releases")
		_ = pruneReleases(client, releaseRoot)
	}()

	logger.Info("running post-hook")
	err = runHook(client, releasePath, "post-hook")
	if err != nil {
		return err
	}

	return nil
}

func createFolderHierarchy(client *ssh.Client, releaseRoot string) error {
	paths := []string{
		releaseRoot,
		path.Join(releaseRoot, "tmp"),
		path.Join(releaseRoot, "shared"),
		path.Join(releaseRoot, "releases"),
		path.Join(releaseRoot, "releases", releaseId),
	}

	for _, p := range paths {
		err := makePath(client, p)
		if err != nil {
			return err
		}
	}

	return nil
}

func symlinkSharedFiles(client *ssh.Client, releasePath string, sharedPath string) error {
	sharedFiles, err := listSharedFiles(client, releasePath)

	if err != nil {
		return err
	}

	for _, filePath := range sharedFiles {
		err = symlinkSharedFile(client, releasePath, sharedPath, filePath)
		if err != nil {
			return err
		}
	}

	return nil
}

func listSharedFiles(client *ssh.Client, releasePath string) ([]string, error) {
	stdout, _, err := runCommand(client,
		"cat %s",
		shellescape.Quote(path.Join(releasePath, ".go-deploy", "shared")),
	)

	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(stdout)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	return lines, scanner.Err()
}

func symlinkSharedFile(client *ssh.Client, releasePath string, sharedPath string, filePath string) error {
	err := safeRm(client, path.Join(releasePath, filePath))
	if err != nil {
		return err
	}

	_, _, err = runCommand(client,
		"mkdir -p $(dirname %s) && ln -s %s %s",
		shellescape.Quote(path.Join(releasePath, filePath)),
		shellescape.Quote(path.Join(sharedPath, filePath)),
		shellescape.Quote(path.Join(releasePath, filePath)),
	)

	return err
}

func makePath(client *ssh.Client, target string) error {
	_, _, err := runCommand(client,
		"mkdir -p %s && touch %s",
		shellescape.Quote(target),
		shellescape.Quote(target),
	)

	return err
}

func untarFile(client *ssh.Client, tarPath string, releasePath string) error {
	_, _, err := runCommand(client,
		"/usr/bin/env tar xf %s --strip-components=1 -C %s",
		shellescape.Quote(tarPath),
		shellescape.Quote(releasePath+"/"),
	)

	return err
}

func runHook(client *ssh.Client, releasePath string, hookName string) error {
	_, _, err := runCommand(client,
		"cd %s ; if [ -f %s ] ; then exec %s ; fi",
		shellescape.Quote(releasePath),
		path.Join(releasePath, ".go-deploy", hookName),
		path.Join(releasePath, ".go-deploy", hookName),
	)

	return err
}

func swapSymlinks(client *ssh.Client, releaseRoot string, releasePath string) error {
	previousLnPath := path.Join(releaseRoot, "previous")
	currentLnPath := path.Join(releaseRoot, "current")
	tmpLnPath := path.Join(releaseRoot, "current.new")

	_, _, err := runCommand(client,
		"rm %s ; cp -P %s %s ; ln -s %s %s && mv -T %s %s",
		shellescape.Quote(previousLnPath),
		shellescape.Quote(currentLnPath),
		shellescape.Quote(previousLnPath),
		shellescape.Quote(releasePath),
		shellescape.Quote(tmpLnPath),
		shellescape.Quote(tmpLnPath),
		shellescape.Quote(currentLnPath),
	)

	return err
}

func pruneReleases(client *ssh.Client, releaseRoot string) error {
	releases, err := findReleases(client, releaseRoot)

	if err != nil {
		return err
	}

	if len(releases) < keep {
		return nil
	}

	for _, release := range releases[keep:] {
		p := path.Join(releaseRoot, "releases", release)

		err = safeRm(client, p)

		if err != nil {
			return err
		}
	}

	return nil
}

func findReleases(client *ssh.Client, root string) ([]string, error) {
	stdout, _, err := runCommand(client,
		"ls -1t %s",
		shellescape.Quote(path.Join(root, "releases")),
	)
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(stdout)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	return lines, scanner.Err()
}

func safeRm(client *ssh.Client, path string) error {
	_, _, err := runCommand(client,
		"if [ -f %s ] || [ -d %s ] ; then find %s -type f -delete -or -type l -delete ; fi && if [ -d %s ] ; then find %s -type d -delete ; fi",
		shellescape.Quote(path),
		shellescape.Quote(path),
		shellescape.Quote(path),
		shellescape.Quote(path),
		shellescape.Quote(path),
	)

	return err
}

func makeClient(config url.URL) (*ssh.Client, error) {
	hostname := config.Hostname()

	port := config.Port()

	if port == "" {
		port = "22"
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve user home directory: %s", err)
	}

	hostKeyCallback, err := knownhosts.New(path.Join(homeDir, ".ssh", "known_hosts"))
	if err != nil {
		return nil, fmt.Errorf("could not create hostkeycallback function: %s", err)
	}

	sshAuthSock := os.Getenv("SSH_AUTH_SOCK")
	if sshAuthSock == "" {
		return nil, errors.New("the SSH_AUTH_SOCK environment variable is not set, which normally means that no SSH Agent is running")
	}

	conn, err := net.Dial("unix", sshAuthSock)
	if err != nil {
		return nil, fmt.Errorf(
			"Error connecting to agent\n\n"+
				"The agent address is detected using the SSH_AUTH_SOCK environment\n"+
				"variable. Please verify this variable is correct and the SSH agent\n"+
				"is properly set up.\n\n%s",
			err,
		)
	}
	defer func(conn net.Conn) {
		_ = conn.Close()
	}(conn)

	a := agent.NewClient(conn)
	signers, err := a.Signers()
	if err != nil {
		return nil, fmt.Errorf("failed loading keys from ssh agent: %s", err)
	}

	client, err := ssh.Dial(
		"tcp",
		strings.Join([]string{hostname, ":", port}, ""),
		&ssh.ClientConfig{
			User:            config.User.Username(),
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signers...)},
			HostKeyCallback: hostKeyCallback,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial ssh server: %s", err)
	}

	return client, nil
}

func copyFile(sshClient *ssh.Client, source string, dest string) error {
	client, err := scp.NewClientBySSH(sshClient)

	if err != nil {
		return err
	}

	err = client.Connect()
	if err != nil {
		return err
	}
	defer client.Close()

	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func(f *os.File) {
		_ = f.Close()
	}(f)

	return client.CopyFromFile(context.Background(), *f, dest, "0440")
}

func runCommand(client *ssh.Client, format string, a ...interface{}) (io.Reader, io.Reader, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, nil, nil
	}
	defer func(session *ssh.Session) {
		_ = session.Close()
	}(session)

	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, nil, nil
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		return nil, nil, nil
	}

	cmd := fmt.Sprintf(format, a...)
	log.Debug(cmd)
	err = session.Run(cmd)

	if err != nil {
		scanner := bufio.NewScanner(io.MultiReader(stdout, stderr))
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}

		log.Error(strings.Join(lines, "\n"))
	}

	return stdout, stderr, err
}
