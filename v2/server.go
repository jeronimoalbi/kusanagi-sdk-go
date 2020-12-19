// Go SDK for the KUSANAGI(tm) framework (http://kusanagi.io)
// Copyright (c) 2016-2020 KUSANAGI S.L. All rights reserved.
//
// Distributed under the MIT license.
//
// For the full copyright and license information, please view the LICENSE
// file that was distributed with this source code.

package kusanagi

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kusanagi/kusanagi-sdk-go/v2/lib"
	"github.com/kusanagi/kusanagi-sdk-go/v2/lib/cli"
	"github.com/kusanagi/kusanagi-sdk-go/v2/lib/log"
	"github.com/kusanagi/kusanagi-sdk-go/v2/lib/payload"
	"github.com/pebbe/zmq4"
)

// State contains the context data for a multipart request of the framework.
type state struct {
	id             string
	componentTitle string
	action         string
	schemas        *payload.Mapping
	command        payload.Command
	reply          *payload.Reply
	payload        []byte
	input          cli.Input
	context        context.Context
	logger         log.RequestLogger
}

// Output for a request
type requestOutput struct {
	state    *state
	err      error
	response responseMsg
}

// Request processor processes ZMQ request messages for a component.
type requestProcessor func(Component, *state, chan<- requestOutput)

// Create a response that contains an error as payload.
func createErrorRespose(rid, message string) (responseMsg, error) {
	p := payload.NewErrorReply()
	p.Error.Message = message
	data, err := lib.Pack(p)
	if err != nil {
		return nil, err
	}
	return responseMsg{[]byte(rid), emptyFrame, data}, nil

}

// Cast the processor output results to interfaces.
func pipeOutput(c <-chan requestOutput) <-chan interface{} {
	pipe := make(chan interface{}, cap(c))

	go func() {
		for output := range c {
			pipe <- output
		}
	}()

	return pipe
}

// Creates a new component server.
func newServer(c Component, p requestProcessor) (*server, error) {
	// Read CLI input values
	input, err := cli.Parse()
	if err != nil {
		return nil, err
	}

	// Setup the log level before the server is created
	log.SetLevel(input.GetLogLevel())
	return &server{c, input, p}, nil
}

// SDK component server.
type server struct {
	component Component
	input     cli.Input
	processor requestProcessor
}

// Get the ZMQ channel address to use for listening incoming requests.
func (s *server) getAddress() (address string) {
	if s.input.IsTCPEnabled() {
		address = fmt.Sprintf("tcp://127.0.0.1:%d", s.input.GetTCP())
	} else if name := s.input.GetSocket(); name != "" {
		address = fmt.Sprintf("ipc://%s", name)
	} else {
		// Create a default name for the socket when no name is available.
		// The 'ipc://' prefix is removed from the string to get the socket name.
		address = lib.IPC(s.input.GetComponent(), s.input.GetName(), s.input.GetVersion())
	}
	return address
}

func (s *server) startMessageListener(msgc <-chan requestMsg) <-chan requestOutput {
	// Create a buffered channel to receive the responses from the handlers
	resc := make(chan requestOutput, 1000)

	// Get the title to use for the component
	title := s.input.GetComponentTitle()

	// Handle messages until the messages channel is closed
	go func() {
		// TODO: See how to avoid race conditions when mapping are updated here (and read by userland)
		var schemas *payload.Mapping

		// Process execution timeout
		timeout := time.Duration(s.input.GetTimeout()) * time.Millisecond

		// Define a parent context for each request
		ctx := context.Background()

		for {
			// Block until a request message is received
			msg, closed := <-msgc
			// When the channel is closed finish the loop
			if closed {
				break
			}

			// Check that the multipart message is valid
			if err := msg.check(); err != nil {
				log.Critical(err)
				// Log the error and continue listening for incoming requests
				continue
			}

			// Try to read the new schemas when present
			if v := msg.getSchemas(); v != nil {
				if err := lib.Unpack(v, &schemas); err != nil {
					log.Errorf("Failed to read schemas: %v", err)
				}
			}

			// Process the request message in a new goroutine
			// TODO: Move to a function
			go func() {
				rid := msg.getRequestID()
				action := msg.getAction()
				logger := log.NewRequestLogger(rid)

				// State for the request
				state := state{
					id:      rid,
					action:  action,
					schemas: schemas,
					input:   s.input,
					logger:  logger,
				}

				// Prepare defaults for the request output
				output := requestOutput{state: &state}

				// Check that the request action is defined
				if c := s.component.(*component); !c.hasCallback(msg.getAction()) {
					output.err = fmt.Errorf(`Invalid action for component %s: "%s"`, title, action)
					resc <- output
					return

				}

				// Try to read the new schemas when present
				if v := msg.getPayload(); v != nil {
					if err := lib.Unpack(v, state.command); err != nil {
						log.Criticalf("Failed to read payload: %v", err)
						output.err = fmt.Errorf(`Invalid payload for component %s: "%s"`, title, action)
						resc <- output
						return
					}
				}

				// Create a child context with the process execution timeout as limit
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				state.context = ctx

				// Create a channel to wait for the processor output
				outc := make(chan requestOutput)
				defer close(outc)

				// Process the request and return the response
				go s.processor(s.component, &state, outc)

				// Block until the processor finishes or the execution timeout is triggered
				select {
				case output := <-outc:
					resc <- output
				case <-ctx.Done():
					logger.Warningf("Execution timed out after %dms. PID: %d", timeout, os.Getpid())
				}
			}()
		}
	}()

	return resc
}

func (s *server) start() error {
	// Define a custom ZMQ context
	zctx, err := zmq4.NewContext()
	if err != nil {
		return err
	}

	// Listen for termination signals
	go func() {
		// Define a channel to receive system signals
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
		// Block until a signal is received
		<-sigc
		log.Debug("Termination signal received")
		// Terminate the ZMQ context to close sockets gracefully
		if err := zctx.Term(); err != nil {
			log.Errorf("Failed to terminate sockets context: %v", err)
		} else {
			log.Debug("Socket context terminated successfully")
		}
		// Clear the default ZMQ settings for retrying operations after EINTR.
		zmq4.SetRetryAfterEINTR(false)
		zctx.SetRetryAfterEINTR(false)
	}()

	// Create a socket to receive incoming requests
	socket, err := zctx.NewSocket(zmq4.ROUTER)
	if err != nil {
		return fmt.Errorf("Failed to create socket: %v", err)
	}
	defer socket.Close()

	// Make sure sockets close after context is terminated
	if err := socket.SetLinger(0); err != nil {
		return fmt.Errorf("Failed to set socket's linger option: %v", err)
	}
	// Set the maximum number of request that are cached by the socket.
	// ZMQ default value is 1000.
	if err := socket.SetRcvhwm(1000); err != nil {
		return fmt.Errorf("Failed to set socket's high water mark option: %v", err)
	}

	// Start listening for incoming requests
	address := s.getAddress()
	log.Debugf(`Listening for request at address: "%s"`, address)
	if err := socket.Bind(address); err != nil {
		return fmt.Errorf(`Faled to open socket at address "%s": %v`, address, err)
	}
	defer socket.Unbind(address)

	// Create a buffered channel to send request payloads to the message listener.
	// The channel is buffered to allow faster request processing by the reactor.
	msgc := make(chan requestMsg, 1000)
	// On exit close the channel to avoid worker creation
	defer close(msgc)
	// Define a channel to read the responses from the processors.
	// Note: The output is piped to be able to use the channel in the ZMQ reactor.
	resc := pipeOutput(s.startMessageListener(msgc))

	// Create a reactor to handle requests and worker responses.
	// The reactor will handle as many responses as possible before reading incoming requests.
	// Request are cached by ZMQ until the high water mark is reached.
	// The reactor processes a handler at a time, so it is important that the socket and channel
	// handlers finish as fast as possible.
	reactor := zmq4.NewReactor()
	reactor.AddSocket(socket, zmq4.POLLIN, func(zmq4.State) error {
		// When a request is recieved read it and add it to the messages channel
		// so the workers can process it.
		msg, err := socket.RecvMessageBytes(0)
		if err != nil {
			// When the context is terminated return the error to stop the reactor
			if zmq4.AsErrno(err) == zmq4.ETERM {
				return err
			} else {
				log.Errorf("Failed to read request payload: %v", err)
			}
		}
		msgc <- msg
		return nil
	})
	reactor.AddChannel(resc, -1, func(v interface{}) error {
		output := v.(requestOutput)
		logger := output.state.logger
		response := output.response
		if output.err != nil {
			// Create an error response
			response, err = createErrorRespose(output.state.id, output.err.Error())
			if err != nil {
				// When the error response creation fails log the issue
				// and stop processing the response.
				logger.Errorf("Request failed with error: %v", output.err)
				logger.Errorf("Failed to create error response: %v", err)
				return nil
			}
		}

		if _, err := socket.SendMessage(response); err != nil {
			// When the context is terminated return the error to stop the reactor
			if zmq4.AsErrno(err) == zmq4.ETERM {
				return err
			} else {
				logger.Errorf("Failed to send response to client: %v", err)
			}
		}
		return nil
	})
	reactor.Run(time.Second)
	log.Info("Component stopped")
	return nil
}
