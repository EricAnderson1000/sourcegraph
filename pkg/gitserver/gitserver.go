package gitserver

import (
	"bytes"
	"errors"
	"log"
	"net/rpc"
	"os"
	"os/exec"
	"path"
	"syscall"
	"time"

	"src.sourcegraph.com/sourcegraph/pkg/vcs"
)

type Git struct {
}

type ExecArgs struct {
	Repo  string
	Args  []string
	Opt   *vcs.RemoteOpts
	Stdin []byte
}

type ExecReply struct {
	RepoExists bool
	Error      string
	ExitStatus int
	Stdout     []byte
	Stderr     []byte
}

var ReposDir string
var callChan chan<- *rpc.Call

func RegisterHandler() {
	rpc.Register(&Git{})
	rpc.HandleHTTP()
}

func (g *Git) Exec(args *ExecArgs, reply *ExecReply) error {
	dir := path.Join(ReposDir, args.Repo)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	reply.RepoExists = true

	cmd := exec.Command("git", args.Args...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(args.Stdin)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if args.Opt != nil && args.Opt.SSH != nil {
		gitSSHWrapper, gitSSHWrapperDir, keyFile, err := makeGitSSHWrapper(args.Opt.SSH.PrivateKey)
		defer func() {
			if keyFile != "" {
				if err := os.Remove(keyFile); err != nil {
					log.Fatalf("Error removing SSH key file %s: %s.", keyFile, err)
				}
			}
		}()
		if err != nil {
			return err
		}
		defer os.Remove(gitSSHWrapper)
		if gitSSHWrapperDir != "" {
			defer os.RemoveAll(gitSSHWrapperDir)
		}
		cmd.Env = []string{"GIT_SSH=" + gitSSHWrapper}
	}

	if args.Opt != nil && args.Opt.HTTPS != nil {
		env := environ(os.Environ())
		env.Unset("GIT_TERMINAL_PROMPT")

		gitPassHelper, gitPassHelperDir, err := makeGitPassHelper(args.Opt.HTTPS.Pass)
		if err != nil {
			return err
		}
		defer os.Remove(gitPassHelper)
		if gitPassHelperDir != "" {
			defer os.RemoveAll(gitPassHelperDir)
		}
		env.Unset("GIT_ASKPASS")
		env = append(env, "GIT_ASKPASS="+gitPassHelper)

		cmd.Env = env
	}

	if err := cmd.Run(); err != nil {
		reply.Error = err.Error()
	}
	if cmd.ProcessState != nil { // is nil if process failed to start
		reply.ExitStatus = cmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus()
	}
	reply.Stdout = stdoutBuf.Bytes()
	reply.Stderr = stderrBuf.Bytes()
	return nil
}

func Dial(addr string) error {
	clientSingleton, err := rpc.DialHTTP("tcp", addr)
	if err != nil {
		return err
	}

	theCallChan := make(chan *rpc.Call, 10)
	callChan = theCallChan
	resetConnectionChan := make(chan *rpc.Client)

	go func() {
		for {
			select {
			case call := <-theCallChan:
				clientForCall := clientSingleton
				done := make(chan *rpc.Call, 1)
				clientForCall.Go(call.ServiceMethod, call.Args, call.Reply, done)
				go func() {
					call.Error = (<-done).Error
					if call.Error == rpc.ErrShutdown {
						resetConnectionChan <- clientForCall
						callChan <- call // retry
						return
					}
					call.Done <- call
				}()

			case client := <-resetConnectionChan:
				if client != clientSingleton {
					continue
				}
				clientSingleton.Close()
				for {
					newClient, err := rpc.DialHTTP("tcp", addr)
					if err != nil {
						log.Printf("dial to git server failed: %s", err)
						time.Sleep(time.Second)
						continue
					}
					clientSingleton = newClient
					break
				}
			}
		}
	}()

	return nil
}

func call(serviceMethod string, args interface{}, reply interface{}) error {
	done := make(chan *rpc.Call, 1)
	callChan <- &rpc.Call{ServiceMethod: serviceMethod, Args: args, Reply: reply, Done: done}
	return (<-done).Error
}

type Cmd struct {
	Args       []string
	Repo       string
	Opt        *vcs.RemoteOpts
	Input      []byte
	ExitStatus int
}

func Command(name string, arg ...string) *Cmd {
	if name != "git" {
		panic("gitserver: command name must be 'git'")
	}
	return &Cmd{
		Args: append([]string{"git"}, arg...),
	}
}

func (c *Cmd) DividedOutput() ([]byte, []byte, error) {
	var reply ExecReply
	if err := call("Git.Exec", &ExecArgs{Repo: c.Repo, Args: c.Args[1:], Opt: c.Opt, Stdin: c.Input}, &reply); err != nil {
		return nil, nil, err
	}
	if !reply.RepoExists {
		return nil, nil, vcs.ErrRepoNotExist
	}
	var err error
	if reply.Error != "" {
		err = errors.New(reply.Error)
	}
	c.ExitStatus = reply.ExitStatus
	return reply.Stdout, reply.Stderr, err
}

func (c *Cmd) Run() error {
	_, _, err := c.DividedOutput()
	return err
}

func (c *Cmd) Output() ([]byte, error) {
	stdout, _, err := c.DividedOutput()
	return stdout, err
}

func (c *Cmd) CombinedOutput() ([]byte, error) {
	stdout, stderr, err := c.DividedOutput()
	return append(stdout, stderr...), err
}
