package dexec

import (
	"bytes"
	"errors"
	docker "github.com/fsouza/go-dockerclient"
	"fmt"
	"io"
)

// Process represents a running process in a Docker container.
// It provides methods similar to os.Process for container management.
type Process struct {
	// ContainerID holds the Docker container ID
	ContainerID string
	
	docker Docker
}

// Kill terminates the process by stopping the Docker container.
func (p *Process) Kill() error {
	return p.Signal(docker.SIGKILL)
}

// Signal sends a signal to the process in the Docker container.
// This is similar to os.Process.Signal but sends the signal to the container process.
func (p *Process) Signal(sig docker.Signal) error {
	if p.ContainerID == "" {
		return errors.New("dexec: no container to signal")
	}
	
	err := p.docker.Client.KillContainer(docker.KillContainerOptions{
		ID:     p.ContainerID,
		Signal: sig,
	})
	if err != nil {
		return fmt.Errorf("dexec: failed to signal container %s: %v", p.ContainerID, err)
	}
	return nil
}

// Docker contains connection to Docker API.
// Use github.com/fsouza/go-dockerclient to initialize *docker.Client.
type Docker struct {
	*docker.Client
}

// Command returns the Cmd struct to execute the named program with given
// arguments using specified execution method.
//
// For each new Cmd, you should create a new instance for "method" argument.
func (d Docker) Command(method Execution, name string, arg ...string) *Cmd {
	return &Cmd{Method: method, Path: name, Args: arg, docker: d}
}

// Cmd represents an external command being prepared or run.
//
// A Cmd cannot be reused after calling its Run, Output or CombinedOutput
// methods.
type Cmd struct {
	// Method provides the execution strategy for the context of the Cmd.
	// An instance of Method should not be reused between Cmds.
	Method Execution

	// Path is the path or name of the command in the container.
	Path string

	// Arguments to the command in the container, excluding the command
	// name as the first argument.
	Args []string

	// Env is environment variables to the command. If Env is nil, Run will use
	// Env specified on Method or pre-built container image.
	Env []string

	// Dir specifies the working directory of the command. If Dir is the empty
	// string, Run uses Dir specified on Method or pre-built container image.
	Dir string

	// Stdin specifies the process's standard input.
	// If Stdin is nil, the process reads from the null device (os.DevNull).
	//
	// Run will not close the underlying handle if the Reader is an *os.File
	// differently than os/exec.
	Stdin io.Reader

	// Stdout and Stderr specify the process's standard output and error.
	// If either is nil, they will be redirected to the null device (os.DevNull).
	//
	// Run will not close the underlying handles if they are *os.File differently
	// than os/exec.
	Stdout io.Writer
	Stderr io.Writer

	// Process is the underlying process, once started.
	Process *Process

	docker         Docker
	started        bool
	closeAfterWait []io.Closer
}

// Start starts the specified command but does not wait for it to complete.
func (c *Cmd) Start() error {
	if c.Dir != "" {
		if err := c.Method.setDir(c.Dir); err != nil {
			return err
		}
	}
	if c.Env != nil {
		if err := c.Method.setEnv(c.Env); err != nil {
			return err
		}
	}

	if c.started {
		return errors.New("dexec: already started")
	}
	c.started = true

	if c.Stdin == nil {
		c.Stdin = empty
	}
	if c.Stdout == nil {
		c.Stdout = io.Discard
	}
	if c.Stderr == nil {
		c.Stderr = io.Discard
	}

	cmd := append([]string{c.Path}, c.Args...)
	if err := c.Method.create(c.docker, cmd); err != nil {
		return err
	}
	if err := c.Method.run(c.docker, c.Stdin, c.Stdout, c.Stderr); err != nil {
		return err
	}
	
	// Initialize the Process field after the container is created and started
	c.Process = &Process{
		ContainerID: c.Method.containerID(),
		docker:      c.docker,
	}
	
	return nil
}

// Wait waits for the command to exit. It must have been started by Start.
//
// If the container exits with a non-zero exit code, the error is of type
// *ExitError. Other error types may be returned for I/O problems and such.
//
// Different than os/exec.Wait, this method will not release any resources
// associated with Cmd (such as file handles).
func (c *Cmd) Wait() error {
	defer closeFds(c.closeAfterWait)
	if !c.started {
		return errors.New("dexec: not started")
	}
	ec, err := c.Method.wait(c.docker)
	if err != nil {
		return err
	}
	if ec != 0 {
		return &ExitError{ExitCode: ec}
	}
	return nil
}

// Run starts the specified command and waits for it to complete.
//
// If the command runs successfully and copying streams are done as expected,
// the error is nil.
//
// If the container exits with a non-zero exit code, the error is of type
// *ExitError. Other error types may be returned for I/O problems and such.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// CombinedOutput runs the command and returns its combined standard output and
// standard error.
//
// Docker API does not have strong guarantees over ordering of messages. For instance:
//
//	>&1 echo out; >&2 echo err
//
// may result in "out\nerr\n" as well as "err\nout\n" from this method.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("dexec: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("dexec: Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout, c.Stderr = &b, &b
	err := c.Run()
	return b.Bytes(), err
}

// Output runs the command and returns its standard output.
//
// If the container exits with a non-zero exit code, the error is of type
// *ExitError. Other error types may be returned for I/O problems and such.
//
// If c.Stderr was nil, Output populates ExitError.Stderr.
func (c *Cmd) Output() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("dexec: Stdout already set")
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout

	captureErr := c.Stderr == nil
	if captureErr {
		c.Stderr = &stderr
	}
	err := c.Run()
	if err != nil && captureErr {
		if ee, ok := err.(*ExitError); ok {
			ee.Stderr = stderr.Bytes()
		}
	}
	return stdout.Bytes(), err
}

// StdinPipe returns a pipe that will be connected to the command's standard input
// when the command starts.
//
// Different than os/exec.StdinPipe, returned io.WriteCloser should be closed by user.
func (c *Cmd) StdinPipe() (io.WriteCloser, error) {
	if c.Stdin != nil {
		return nil, errors.New("dexec: Stdin already set")
	}
	pr, pw := io.Pipe()
	c.Stdin = pr
	return pw, nil
}

// StdoutPipe returns a pipe that will be connected to the command's standard output when
// the command starts.
//
// Wait will close the pipe after seeing the command exit or in error conditions.
func (c *Cmd) StdoutPipe() (io.ReadCloser, error) {
	if c.Stdout != nil {
		return nil, errors.New("dexec: Stdout already set")
	}
	pr, pw := io.Pipe()
	c.Stdout = pw
	c.closeAfterWait = append(c.closeAfterWait, pw)
	return pr, nil
}

// StderrPipe returns a pipe that will be connected to the command's standard error when
// the command starts.
//
// Wait will close the pipe after seeing the command exit or in error conditions.
func (c *Cmd) StderrPipe() (io.ReadCloser, error) {
	if c.Stderr != nil {
		return nil, errors.New("dexec: Stderr already set")
	}
	pr, pw := io.Pipe()
	c.Stderr = pw
	c.closeAfterWait = append(c.closeAfterWait, pw)
	return pr, nil
}

func closeFds(l []io.Closer) {
	for _, fd := range l {
		fd.Close()
	}
}

type emptyReader struct{}

func (r *emptyReader) Read(b []byte) (int, error) { return 0, io.EOF }

var empty = &emptyReader{}
