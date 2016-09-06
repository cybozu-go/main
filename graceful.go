// +build !windows

package cmd

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cybozu-go/log"
	"github.com/pkg/errors"
)

const (
	listenEnv = "CYBOZU_LISTEN_FDS"

	restartWait = 10 * time.Millisecond
)

func isMaster() bool {
	return len(os.Getenv(listenEnv)) == 0
}

func restoreListeners(envvar string) ([]net.Listener, error) {
	nfds, err := strconv.Atoi(os.Getenv(envvar))
	defer os.Unsetenv(envvar)

	if err != nil {
		return nil, err
	}
	if nfds == 0 {
		return nil, nil
	}

	log.Debug("cmd: restored listeners", map[string]interface{}{
		"nfds": nfds,
	})

	ls := make([]net.Listener, 0, nfds)
	for i := 0; i < nfds; i++ {
		fd := 3 + i
		f := os.NewFile(uintptr(fd), "FD"+strconv.Itoa(fd))
		l, err := net.FileListener(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		ls = append(ls, l)
	}
	return ls, nil
}

// SystemdListeners returns listeners from systemd socket activation.
func SystemdListeners() ([]net.Listener, error) {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil {
		return nil, errors.Wrap(err, "SystemdListeners")
	}
	if pid != os.Getpid() {
		return nil, nil
	}
	return restoreListeners("LISTEN_FDS")
}

// Graceful is a struct to implement graceful restart servers.
type Graceful struct {
	// Listen is a function to create listening sockets.
	// This function is called in the master process.
	Listen func() ([]net.Listener, error)

	// Serve is a function to accept connections from listeners.
	// This function is called in child processes.
	// In case of errors, use os.Exit to exit.
	Serve func(listeners []net.Listener)

	// ExitTimeout is duration before Run gives up waiting for
	// a child to exit.  Zero disables timeout.
	ExitTimeout time.Duration

	// Env is the environment for the master process.
	// If nil, the global environment is used.
	Env *Environment
}

// Run runs the graceful restarting server.
//
// If this is the master process, Run starts a child process,
// and installs SIGHUP handler to restarts the child process.
//
// If this is a child process, Run simply calls g.Serve.
//
// Run returns immediately in the master process, and never
// returns in the child process.
func (g *Graceful) Run() {
	if isMaster() {
		env := g.Env
		if env == nil {
			env = defaultEnv
		}
		env.Go(g.runMaster)
		return
	}

	lns, err := restoreListeners(listenEnv)
	if err != nil {
		log.ErrorExit(err)
	}
	log.DefaultLogger().SetDefaults(map[string]interface{}{
		"pid": os.Getpid(),
	})
	log.Info("cmd: new child", nil)
	g.Serve(lns)

	// child process should not return.
	os.Exit(0)
	return
}

// runMaster is the main function of the master process.
func (g *Graceful) runMaster(ctx context.Context) error {
	logger := log.DefaultLogger()

	// prepare listener files
	listeners, err := g.Listen()
	if err != nil {
		return err
	}
	files, err := listenerFiles(listeners)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no listener")
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
		// we cannot close listeners no sooner than this point
		// because net.UnixListener removes the socket file on Close.
		for _, l := range listeners {
			l.Close()
		}
	}()

	sighup := make(chan os.Signal, 2)
	signal.Notify(sighup, syscall.SIGHUP)

RESTART:
	child := g.makeChild(files)
	clog, err := child.StderrPipe()
	if err != nil {
		return err
	}
	copyDone := make(chan struct{})
	go copyLog(logger, clog, copyDone)

	done := make(chan error, 1)
	err = child.Start()
	if err != nil {
		return err
	}
	go func() {
		done <- child.Wait()
	}()

	select {
	case err := <-done:
		<-copyDone
		return err
	case <-sighup:
		child.Process.Signal(syscall.SIGTERM)
		log.Warn("cmd: got sighup", nil)
		time.Sleep(restartWait)
		goto RESTART
	case <-ctx.Done():
		child.Process.Signal(syscall.SIGTERM)
		if g.ExitTimeout == 0 {
			<-done
			<-copyDone
			return nil
		}
		select {
		case <-done:
			<-copyDone
			return nil
		case <-time.After(g.ExitTimeout):
			logger.Warn("cmd: timeout child exit", nil)
			return nil
		}
	}
}

func (g *Graceful) makeChild(files []*os.File) *exec.Cmd {
	child := exec.Command(os.Args[0], os.Args[1:]...)
	child.Env = []string{listenEnv + "=" + strconv.Itoa(len(files))}
	child.ExtraFiles = files
	return child
}

func copyLog(logger *log.Logger, r io.ReadCloser, done chan<- struct{}) {
	defer func() {
		r.Close()
		close(done)
	}()

	var unwritten []byte
	buf := make([]byte, 1<<20)

	for {
		n, err := r.Read(buf)
		if err != nil {
			if len(unwritten) == 0 {
				if n > 0 {
					logger.WriteThrough(buf[0:n])
				}
				return
			}
			unwritten = append(unwritten, buf[0:n]...)
			logger.WriteThrough(unwritten)
			return
		}
		if n == 0 {
			continue
		}
		if buf[n-1] != '\n' {
			unwritten = append(unwritten, buf[0:n]...)
			continue
		}
		if len(unwritten) == 0 {
			err = logger.WriteThrough(buf[0:n])
			if err != nil {
				return
			}
			continue
		}
		unwritten = append(unwritten, buf[0:n]...)
		err = logger.WriteThrough(unwritten)
		if err != nil {
			return
		}
		unwritten = unwritten[:0]
	}
}
