// MIT License
//
// Copyright (c) 2020 Tweag IO
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//

package cmd

import (
	"fmt"
	"github.com/coreos/go-systemd/activation"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/tweag/trustix/config"
	"github.com/tweag/trustix/core"
	"github.com/tweag/trustix/correlator"
	pb "github.com/tweag/trustix/proto"
	"github.com/tweag/trustix/rpc"
	"github.com/tweag/trustix/rpc/auth"
	"google.golang.org/grpc"
	"net"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sync"
	"syscall"
)

var once sync.Once
var configPath string
var stateDirectory string

var listenAddress string
var dialAddress string

var rootCmd = &cobra.Command{
	Use:   "trustix",
	Short: "Trustix",
	Long:  `Trustix`,
	RunE: func(cmd *cobra.Command, args []string) error {

		if configPath == "" {
			return fmt.Errorf("Missing config flag")
		}

		config, err := config.NewConfigFromFile(configPath)
		if err != nil {
			log.Fatal(err)
		}

		log.WithFields(log.Fields{
			"directory": stateDirectory,
		}).Info("Creating state directory")
		err = os.MkdirAll(stateDirectory, 0700)
		if err != nil {
			log.Fatalf("Could not create state directory: %s", stateDirectory)
		}

		flagConfig := &core.FlagConfig{
			StateDirectory: stateDirectory,
		}

		// Check if any names are non-unique
		seenNames := make(map[string]struct{})
		// The number of authoritive logs, can't exceed 1
		numLogs := 0
		for _, logConfig := range config.Logs {
			_, ok := seenNames[logConfig.Name]
			if ok {
				log.Fatalf("Found non-unique log name: %s", logConfig.Name)
			}
			seenNames[logConfig.Name] = struct{}{}

			if logConfig.Mode == "trustix-log" {
				numLogs += 1
				if numLogs > 1 {
					log.Fatal("More than 1 authoritive logs in the same instance is not supported.")
				}
			}
		}

		var rpcServer *rpc.TrustixRPCServer
		var kvServer *rpc.TrustixKVServer

		logMap := make(map[string]*core.TrustixCore)
		for _, logConfig := range config.Logs {
			log.WithFields(log.Fields{
				"name":   logConfig.Name,
				"pubkey": logConfig.Signer.PublicKey,
				"mode":   logConfig.Mode,
			}).Info("Adding log")
			c, err := core.CoreFromConfig(logConfig, flagConfig)
			if err != nil {
				log.Fatal(err)
			}

			logMap[logConfig.Name] = c

			if logConfig.Mode == "trustix-log" {
				log.WithFields(log.Fields{
					"name": logConfig.Name,
					"mode": logConfig.Mode,
				}).Info("Adding authoritive log to gRPC")

				rpcServer = rpc.NewTrustixRPCServer(c)
				kvServer = rpc.NewTrustixKVServer(c)
			}
		}

		corr, err := correlator.NewMinimumPercentCorrelator(50)
		if err != nil {
			log.Fatalf("Failed to create correlator: %v", err)
		}

		logServer := rpc.NewTrustixLogServer(logMap, corr)

		log.Debug("Creating gRPC servers")

		errChan := make(chan error)

		createServer := func(lis net.Listener) (s *grpc.Server) {
			_, isUnix := lis.(*net.UnixListener)

			if isUnix {
				s = grpc.NewServer(
					grpc.Creds(&auth.SoPeercred{}), // Attach SO_PEERCRED auth to UNIX sockets
				)

				pb.RegisterTrustixLogServer(s, logServer)

				if rpcServer != nil {
					pb.RegisterTrustixRPCServer(s, rpcServer)
				}
			} else {
				s = grpc.NewServer()
			}

			if kvServer != nil {
				pb.RegisterTrustixKVServer(s, kvServer)
			}

			go func() {
				err := s.Serve(lis)
				if err != nil {
					errChan <- fmt.Errorf("failed to serve: %v", err)
				}
			}()

			return s
		}

		var servers []*grpc.Server

		// Systemd socket activation
		listeners, err := activation.Listeners()
		if err != nil {
			log.Fatalf("cannot retrieve listeners: %s", err)
		}
		for _, lis := range listeners {
			log.WithFields(log.Fields{
				"address": lis.Addr(),
			}).Info("Using socket activated listener")

			servers = append(servers, createServer(lis))
		}

		// Create sockets
		for _, addr := range []string{listenAddress} {

			if addr == "" {
				continue
			}

			log.WithFields(log.Fields{
				"address": dialAddress,
			}).Info("Listening to address")

			lis, err := net.Listen("tcp", addr)
			if err != nil {
				log.Fatalf("failed to listen: %v", err)
			}

			servers = append(servers, createServer(lis))
		}

		if len(servers) <= 0 {
			log.Fatal("No listeners configured!")
		}

		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			<-quit
			var wg *sync.WaitGroup

			for _, server := range servers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					server.GracefulStop()
				}()
			}

			wg.Wait()

			close(errChan)
		}()

		for err := range errChan {
			log.Fatal(err)
		}

		return nil
	},
}

func initCommands() {
	rootCmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")

	rootCmd.PersistentFlags().StringVar(&listenAddress, "listen", "", "Listen to address")

	trustixSock := os.Getenv("TRUSTIX_SOCK")
	if trustixSock == "" {
		tmpDir := "/tmp"
		trustixSock = filepath.Join(tmpDir, "trustix.sock")
	}

	rootCmd.PersistentFlags().StringVar(&dialAddress, "address", trustixSock, "Connect to address")

	log.SetLevel(log.DebugLevel)

	homeDir, _ := os.UserHomeDir()
	defaultStateDir := path.Join(homeDir, ".local/share/trustix")
	rootCmd.PersistentFlags().StringVar(&stateDirectory, "state", defaultStateDir, "State directory")

	rootCmd.AddCommand(generateKeyCmd)
	initGenerate()

	rootCmd.AddCommand(submitCommand)
	initSubmit()

	rootCmd.AddCommand(queryCommand)
	initQuery()

	rootCmd.AddCommand(queryMap)
	initMapQuery()

	rootCmd.AddCommand(decideCommand)
	initDecide()
}

func Execute() {
	once.Do(initCommands)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
