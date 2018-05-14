package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/OnebookTechnology/BigStone/server"
	"log"
	"os"
	"os/signal"
	"time"
)

var confPath string
var op string

const (
	MaxArgsCount = 4
	// Change your server name
	ServerName = "BigStone"
	PidFileDir = "./run/"
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n"+
		"\t"+ServerName+" [-c confPath] [start|stop]&\n", ServerName)
	flag.PrintDefaults()
}

func init() {
	flag.StringVar(&confPath, "c", "./conf/server.conf", "configuration file path")
	flag.Usage = usage
	if len(os.Args) > MaxArgsCount {
		usage()
		os.Exit(-1)
	}
	op = os.Args[len(os.Args)-1]

}

func checkError(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	flag.Parse()
	fmt.Println(os.Args)
	fmt.Println(op, ServerName, "starting ...")
	fmt.Println("conf file:", confPath)

	switch op {
	case "start":
		server, err := server.NewService(confPath, ServerName)
		checkError(err)

		err = os.MkdirAll(PidFileDir, 0755)
		checkError(err)

		server.Logger.Debug(os.Getppid(), os.Getpid())

		go func() {
			if err := server.Start(); err != nil {
				server.Logger.Error("Server cannot start:", err)
			}
		}()

		// Wait for interrupt signal to gracefully shutdown the server with
		// a timeout of 5 seconds.
		quit := make(chan os.Signal)
		signal.Notify(quit, os.Interrupt)
		<-quit
		log.Println("Shutdown Server ...")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.HttpServer.Shutdown(ctx); err != nil {
			server.Logger.Error("Server Shutdown:", err)
		}
		server.Logger.Info("Server exiting")

	case "stop":
		//

	default:
		usage()
		os.Exit(-1)
	}
}
