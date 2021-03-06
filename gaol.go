package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/codegangsta/cli"
	"github.com/kr/pty"
	"github.com/mattn/go-shellwords"
	"github.com/pivotal-golang/archiver/compressor"
	"github.com/pkg/term"

	"github.com/cloudfoundry-incubator/garden"
	gclient "github.com/cloudfoundry-incubator/garden/client"
	gconn "github.com/cloudfoundry-incubator/garden/client/connection"
)

func handleComplete(c *cli.Context) {
	// This will complete if no args are passed
	if c.Args().Present() {
		return
	}

	containers, err := client(c).Containers(nil)
	failIf(err)

	for _, container := range containers {
		fmt.Println(container.Handle())
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "failed:", err)
	os.Exit(1)
}

func failIf(err error) {
	if err != nil {
		fail(err)
	}
}

func client(c *cli.Context) garden.Client {
	target := c.GlobalString("target")
	return gclient.New(gconn.New("tcp", target))
}

func handle(c *cli.Context) string {
	if len(c.Args()) == 0 {
		fail(errors.New("must provide container handle"))
	}
	return c.Args().First()
}

func main() {
	app := cli.NewApp()
	app.Name = "gaol"
	app.Usage = "a cli for garden"
	app.Version = "0.0.1"
	app.Author = "Chris Brown"
	app.Email = "cbrown@pivotal.io"
	app.EnableBashCompletion = true

	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "target, t",
			Value:  "localhost:7777",
			Usage:  "server to which commands are sent",
			EnvVar: "GAOL_TARGET",
		},
	}

	app.Commands = []cli.Command{
		{
			Name:  "ping",
			Usage: "check if the server is running",
			Action: func(c *cli.Context) {
				err := client(c).Ping()
				failIf(err)
			},
		},
		{
			Name:  "create",
			Usage: "create a container",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "handle, n",
					Usage: "name to give container",
				},
				cli.StringFlag{
					Name:  "rootfs, r",
					Usage: "rootfs image with which to create the container",
				},
				cli.DurationFlag{
					Name:  "grace, g",
					Usage: "grace time (resetting ttl) of container",
				},
				cli.BoolFlag{
					Name:  "privileged, p",
					Usage: "privileged user in container is privileged in host",
				},
			},
			Action: func(c *cli.Context) {
				handle := c.String("handle")
				grace := c.Duration("grace")
				rootfs := c.String("rootfs")
				privileged := c.Bool("privileged")

				container, err := client(c).Create(garden.ContainerSpec{
					Handle:     handle,
					GraceTime:  grace,
					RootFSPath: rootfs,
					Privileged: privileged,
				})
				failIf(err)

				fmt.Println(container.Handle())
			},
		},
		{
			Name:         "destroy",
			Usage:        "destroy a container",
			BashComplete: handleComplete,
			Action: func(c *cli.Context) {
				client := client(c)
				handles := c.Args()

				for _, handle := range handles {
					err := client.Destroy(handle)
					failIf(err)
				}
			},
		},
		{
			Name:  "list",
			Usage: "get a list of running containers",
			Action: func(c *cli.Context) {
				containers, err := client(c).Containers(nil)
				failIf(err)

				for _, container := range containers {
					fmt.Println(container.Handle())
				}
			},
		},
		{
			Name:  "run",
			Usage: "run a command in a container",
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:  "attach, a",
					Usage: "attach to the process after it is started",
				},
				cli.StringFlag{
					Name:  "dir, d",
					Usage: "current working directory of process",
				},
				cli.StringFlag{
					Name:  "user, u",
					Usage: "user to run the process as",
				},
				cli.BoolFlag{
					Name:  "privileged, p",
					Usage: "use privileged user in container",
				},
			},
			BashComplete: handleComplete,
			Action: func(c *cli.Context) {
				attach := c.Bool("attach")
				dir := c.String("dir")
				user := c.String("user")
				privileged := c.Bool("privileged")

				handle := handle(c)
				container, err := client(c).Lookup(handle)
				failIf(err)

				var processIo garden.ProcessIO
				if attach {
					processIo = garden.ProcessIO{
						Stdin:  os.Stdin,
						Stdout: os.Stdout,
						Stderr: os.Stderr,
					}
				} else {
					processIo = garden.ProcessIO{}
				}

				command := c.Args()[1]
				args, err := shellwords.Parse(command)
				failIf(err)

				process, err := container.Run(garden.ProcessSpec{
					Path:       args[0],
					Args:       args[1:],
					Dir:        dir,
					Privileged: privileged,
					User:       user,
				}, processIo)
				failIf(err)

				if attach {
					_, err = process.Wait()
					failIf(err)
				} else {
					fmt.Println(process.ID())
				}
			},
		},
		{
			Name:  "attach",
			Usage: "attach to command running in the container",
			Flags: []cli.Flag{
				cli.IntFlag{
					Name:  "pid, p",
					Usage: "process id to connect to",
				},
			},
			BashComplete: handleComplete,
			Action: func(c *cli.Context) {
				pid := uint32(c.Int("pid"))

				handle := handle(c)
				container, err := client(c).Lookup(handle)
				failIf(err)

				process, err := container.Attach(pid, garden.ProcessIO{
					Stdin:  os.Stdin,
					Stdout: os.Stdout,
					Stderr: os.Stderr,
				})
				failIf(err)

				_, err = process.Wait()
				failIf(err)
			},
		},
		{
			Name:         "shell",
			Usage:        "open a shell inside the running container",
			BashComplete: handleComplete,
			Action: func(c *cli.Context) {
				container, err := client(c).Lookup(handle(c))
				failIf(err)

				term, err := term.Open(os.Stdin.Name())
				failIf(err)

				err = term.SetRaw()
				failIf(err)

				rows, cols, err := pty.Getsize(os.Stdin)
				failIf(err)

				process, err := container.Run(garden.ProcessSpec{
					Path: "/bin/sh",
					Args: []string{"-l"},
					Env:  []string{"TERM=" + os.Getenv("TERM")},
					TTY: &garden.TTYSpec{
						WindowSize: &garden.WindowSize{
							Rows:    rows,
							Columns: cols,
						},
					},
					Privileged: true,
				}, garden.ProcessIO{
					Stdin:  term,
					Stdout: term,
					Stderr: term,
				})
				if err != nil {
					term.Restore()
					failIf(err)
				}

				resized := make(chan os.Signal, 10)
				signal.Notify(resized, syscall.SIGWINCH)

				go func() {
					for {
						<-resized

						rows, cols, err := pty.Getsize(os.Stdin)
						if err == nil {
							process.SetTTY(garden.TTYSpec{
								WindowSize: &garden.WindowSize{
									Rows:    rows,
									Columns: cols,
								},
							})
						}
					}
				}()

				process.Wait()
				term.Restore()
			},
		},
		{
			Name:  "stream-in",
			Usage: "stream data into the container",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "to-file, t",
					Usage: "destination path in the container",
				},
			},
			BashComplete: handleComplete,
			Action: func(c *cli.Context) {
				handle := handle(c)

				dst := c.String("to-file")
				if dst == "" {
					fail(errors.New("missing --to-file argument"))
				}

				container, err := client(c).Lookup(handle)
				failIf(err)

				// perform dance to get correct file names
				tmpDir, err := ioutil.TempDir("", "gaol")
				failIf(err)
				defer os.RemoveAll(tmpDir)

				tmp, err := os.Create(filepath.Join(tmpDir, filepath.Base(dst)))
				failIf(err)

				_, err = io.Copy(tmp, os.Stdin)
				failIf(err)

				err = tmp.Close()
				failIf(err)

				reader, writer := io.Pipe()
				go func(w io.WriteCloser) {
					err := compressor.WriteTar(tmp.Name(), w)
					failIf(err)
					w.Close()
				}(writer)

				err = container.StreamIn(filepath.Dir(dst), reader)
				failIf(err)
			},
		},
		{
			Name:  "stream-out",
			Usage: "stream data out of the container",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "from-file, f",
					Usage: "source path in the container",
				},
			},
			BashComplete: handleComplete,
			Action: func(c *cli.Context) {
				handle := handle(c)

				src := c.String("from-file")
				if src == "" {
					fail(errors.New("missing --from-file argument"))
				}

				container, err := client(c).Lookup(handle)
				failIf(err)

				output, err := container.StreamOut(src)
				failIf(err)

				tr := tar.NewReader(output)
				_, err = tr.Next()
				failIf(err)

				_, err = io.Copy(os.Stdout, tr)
				failIf(err)
			},
		},
		{
			Name:  "net-in",
			Usage: "map a port on the host to a port in the container",
			Flags: []cli.Flag{
				cli.IntFlag{
					Name:  "port, p",
					Usage: "container port",
				},
			},
			BashComplete: handleComplete,
			Action: func(c *cli.Context) {
				target := c.GlobalString("target")
				requestedContainerPort := uint32(c.Int("port"))

				if target == "" {
					fail(errors.New("target must be set"))
				}

				handle := handle(c)
				container, err := client(c).Lookup(handle)
				failIf(err)

				hostPort, _, err := container.NetIn(0, requestedContainerPort)
				failIf(err)

				host, _, err := net.SplitHostPort(target)
				failIf(err)

				fmt.Println(net.JoinHostPort(host, fmt.Sprintf("%d", hostPort)))
			},
		},
	}

	app.Run(os.Args)
}
