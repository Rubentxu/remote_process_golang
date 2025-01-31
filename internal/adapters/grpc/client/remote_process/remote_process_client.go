package remote_process_client

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	pb "dev.rubentxu.devops-platform/adapters/grpc/protos/remote_process"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client encapsulates the gRPC client functionality for RemoteProcess
type Client struct {
	client pb.RemoteProcessServiceClient
	conn   *grpc.ClientConn
}

// New creates a new instance of the client
func New(serverAddress string) (*Client, error) {
	conn, err := grpc.NewClient(
		serverAddress,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server: %v", err)
	}

	client := pb.NewRemoteProcessServiceClient(conn)
	return &Client{client: client, conn: conn}, nil
}

// StartProcess sends a request to start a process on the server and receives the output via a channel
func (c *Client) StartProcess(ctx context.Context, processID string, command []string, env map[string]string, workingDir string, outputChan chan<- *pb.ProcessOutput) error {
	// Create the stream
	stream, err := c.client.StartProcess(ctx)
	if err != nil {
		return fmt.Errorf("error creating stream: %v", err)
	}

	// Send the initial request
	err = stream.Send(&pb.ProcessStartRequest{
		ProcessId:        processID,
		Command:          command,
		Environment:      env,
		WorkingDirectory: workingDir,
	})
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}

	// Close the send stream
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("error closing send stream: %v", err)
	}

	// Process responses in a goroutine
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Recovered in StartProcess: %v", r)
			}
			close(outputChan)
		}()
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return // End of stream
			}
			if err != nil {
				log.Printf("Error receiving response: %v", err)
				return
			}

			// Send the output to the channel
			outputChan <- resp
		}
	}()

	return nil
}

// StopProcess sends a request to stop a process on the server
func (c *Client) StopProcess(ctx context.Context, processID string) (bool, string, error) {
	request := &pb.ProcessStopRequest{
		ProcessId: processID,
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	response, err := c.client.StopProcess(ctx, request)
	if err != nil {
		return false, "", fmt.Errorf("error stopping process: %v", err)
	}

	return response.Success, response.Message, nil
}

// MonitorHealth inicia el monitoreo de la salud de un proceso
func (c *Client) MonitorHealth(ctx context.Context, processID string, checkInterval int64, healthChan chan<- *pb.HealthStatus) error {
	stream, err := c.client.MonitorHealth(ctx)
	if err != nil {
		return fmt.Errorf("error creating stream: %v", err)
	}

	// Enviar la solicitud inicial
	err = stream.Send(&pb.HealthCheckRequest{
		ProcessId:     processID,
		CheckInterval: checkInterval,
	})
	if err != nil {
		return fmt.Errorf("error sending request: %v", err)
	}

	// Procesar respuestas en una goroutine
	go func() {
		// Usar un defer recover para manejar posibles pánicos
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Recovered in MonitorHealth: %v", r)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				// El contexto fue cancelado, salir limpiamente
				return
			default:
				resp, err := stream.Recv()
				if err == io.EOF {
					return
				}
				if err != nil {
					if ctx.Err() == context.Canceled {
						// Contexto cancelado, salir silenciosamente
						return
					}
					log.Printf("Error receiving response: %v", err)
					select {
					case healthChan <- &pb.HealthStatus{
						ProcessId: processID,
						IsRunning: false,
						Status:    fmt.Sprintf("Error receiving response: %v", err),
					}:
					case <-ctx.Done():
					}
					return
				}

				// Enviar el estado de salud al canal
				select {
				case healthChan <- resp:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return nil
}

// Close closes the gRPC connection
func (c *Client) Close() {
	if err := c.conn.Close(); err != nil {
		log.Printf("Error closing connection: %v", err)
	}
}
