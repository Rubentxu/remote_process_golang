package websockets

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dev.rubentxu.devops-platform/worker/internal/adapters/worker"
	"dev.rubentxu.devops-platform/worker/internal/domain"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1024
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // En producción, restringir a orígenes válidos
	},
}

type WSHandler struct {
	worker *worker.Worker
}

func NewWSHandler(w *worker.Worker) *WSHandler {
	return &WSHandler{worker: w}
}

// @title Worker WebSocket API
// @version 1.0
// @description API WebSocket para gestionar tareas
// @BasePath /

// HandleConnection godoc
// @Summary Gestiona conexiones WebSocket para tareas
// @Description Endpoint WebSocket para gestionar tareas en tiempo real. Soporta las siguientes acciones:
// @Description - create_task: Crear una nueva tarea
// @Description - stop_task: Detener una tarea en ejecución
// @Description - list_tasks: Listar todas las tareas
// @Tags WebSocket
// @Accept json
// @Produce json
// @Param client_id query string false "ID del cliente para tracking"
// @Success 101 {string} string "Switching Protocols"
// @Failure 400 {object} ErrorResponse
// @Router /ws [get]
func (h *WSHandler) HandleConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer func() {
		// Cierre gradual con timeout extendido
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Closing normally"),
			time.Now().Add(3*time.Second),
		)
		conn.Close()
	}()

	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)

	conn.SetReadLimit(maxMessageSize)
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	go h.sendPing(ctx, conn)

	for {
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		switch msg.Action {
		case "create_task":
			h.handleCreateTask(ctx, conn, msg.Payload)
		case "stop_task":
			h.handleStopTask(ctx, conn, msg.Payload)
		case "list_tasks":
			h.handleListTasks(ctx, conn)
		default:
			h.sendError(conn, "unknown_action", "Unsupported action type")
		}
	}
}

func (h *WSHandler) handleCreateTask(ctx context.Context, conn *websocket.Conn, payload json.RawMessage) {
	var req TaskRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		h.sendError(conn, "invalid_request", "Error decoding task request")
		return
	}

	if req.Name == "" || req.Image == "" {
		h.sendError(conn, "validation_error", "Name and Image are required fields")
		return
	}

	task, taskCtx := h.createTaskFromRequest(ctx, req)
	outputChan, err := h.worker.AddTask(taskCtx, task)
	if err != nil {
		h.sendError(conn, "create_error", fmt.Sprintf("Error creating task: %v", err))
		return
	}

	// Leer del canal y enviar por WebSocket
	for output := range outputChan {
		resp := TaskResponse{
			TaskID:  task.ID.String(),
			Output:  output.Output,
			IsError: output.IsError,
			Status:  output.Status.String(),
		}

		payload, err := json.Marshal(resp)
		if err != nil {
			log.Printf("Error serializando respuesta: %v", err)
			return
		}

		msg := WSMessage{
			Action:  "task_output",
			Payload: json.RawMessage(payload),
		}

		if err := conn.WriteJSON(msg); err != nil {
			log.Printf("Error sending WebSocket message: %v", err)
			return
		}
	}

	// CUANDO el proceso finaliza, enviar un mensaje final "done"
	doneMsg := map[string]interface{}{
		"done":      true,
		"exit_code": 0,
		"message":   "Process completed successfully",
		"status":    domain.FINISHED.String(),
	}
	if err := conn.WriteJSON(doneMsg); err != nil {
		log.Printf("Error sending done message: %v", err)
	}

	// (Opcional) Enviar un CloseMessage indicando cierre normal
	_ = conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Process finished"),
	)
}

func (h *WSHandler) createTaskFromRequest(parent context.Context, req TaskRequest) (domain.Task, context.Context) {
	taskCtx, cancel := context.WithCancel(parent)
	if req.Timeout > 0 {
		taskCtx, cancel = context.WithTimeout(taskCtx, time.Duration(req.Timeout)*time.Second)
	}

	go func() {
		<-taskCtx.Done()
		cancel()
	}()

	return domain.Task{
		ID:    uuid.New(),
		Name:  req.Name,
		State: domain.Scheduled,
		WorkerSpec: domain.WorkerSpec{
			Type:       domain.InstanceType(req.InstanceType),
			Image:      req.Image,
			Command:    req.Command,
			Env:        req.Env,
			WorkingDir: req.WorkingDir,
		},
		CreatedAt: time.Now(),
	}, taskCtx
}

func (h *WSHandler) streamTaskOutput(ctx context.Context, conn *websocket.Conn, taskID string, outputChan <-chan *domain.ProcessOutput) {
	defer h.sendTaskCompletion(ctx, conn, taskID)

	for output := range outputChan {
		// Enviar cada output inmediatamente por el WebSocket
		if !h.sendJSON(conn, "task_output", TaskResponse{
			TaskID:  taskID,
			Output:  output.Output,
			IsError: output.IsError,
			Status:  output.Status.String(),
		}) {
			log.Printf("Error enviando output, cerrando conexión para tarea %s", taskID)
			return
		}

		// Verificar periodicamente si el contexto fue cancelado
		select {
		case <-ctx.Done():
			log.Printf("Contexto cancelado durante streaming de tarea %s", taskID)
			h.worker.StopTask(taskID)
			return
		default:
			// Mantener conexión activa con ping
			conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
		}
	}
}

func (h *WSHandler) sendTaskCompletion(ctx context.Context, conn *websocket.Conn, taskID string) {
	task, err := h.worker.GetTask(taskID)
	if err != nil {
		h.sendError(conn, "task_error", fmt.Sprintf("Error getting task status: %v", err))
		return
	}

	resp := TaskResponse{
		TaskID:      taskID,
		Status:      string(task.State),
		CompletedAt: time.Now().Format(time.RFC3339),
	}

	if task.State == domain.Completed {
		resp.ExitCode = "COMPLETED"
	} else {
		resp.ExitCode = "1"
		resp.Error = "Task failed to complete"
	}

	h.sendJSON(conn, "task_completed", resp)
}

func (h *WSHandler) handleStopTask(ctx context.Context, conn *websocket.Conn, payload json.RawMessage) {
	var req struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		h.sendError(conn, "invalid_request", "Invalid task ID format")
		return
	}

	if err := h.worker.StopTask(req.TaskID); err != nil {
		h.sendError(conn, "stop_error", err.Error())
		return
	}

	h.sendJSON(conn, "task_stopped", TaskResponse{
		TaskID: req.TaskID,
		Status: "stopped",
	})
}

func (h *WSHandler) handleListTasks(ctx context.Context, conn *websocket.Conn) {
	tasks, err := h.worker.GetTasks()
	if err != nil {
		h.sendError(conn, "list_error", "Error retrieving tasks")
		return
	}

	h.sendJSON(conn, "task_list", tasks)
}

func (h *WSHandler) sendPing(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (h *WSHandler) sendJSON(conn *websocket.Conn, action string, data interface{}) bool {
	raw, err := json.Marshal(data)
	if err != nil {
		log.Printf("Error serializando payload: %v", err)
		return false
	}

	msg := WSMessage{
		Action:  action,
		Payload: json.RawMessage(raw),
	}

	conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := conn.WriteJSON(msg); err != nil {
		log.Printf("WebSocket write error: %v", err)
		return false
	}
	return true
}

func (h *WSHandler) sendError(conn *websocket.Conn, code string, message string) {
	h.sendJSON(conn, "task_error", TaskResponse{
		IsError:  true,
		ExitCode: code,
		Error:    message,
	})
}

// @Summary Endpoint de health check
// @Description Retorna el estado de salud del servicio
// @Tags health
// @Accept json
// @Produce json
// @Success 200 {object} HealthResponse
// @Router /health [get]
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	status := map[string]interface{}{
		"timestamp": time.Now(),
		"status":    "up",
		"service":   "orchestrator",
	}
	w.WriteHeader(http.StatusOK)

	json.NewEncoder(w).Encode(status)
}

func HandleSignals() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		log.Println("Signal received, shutting down...")
		os.Exit(0)
	}
}

// WSMessage representa un mensaje WebSocket
// swagger:model
type WSMessage struct {
	// Acción a realizar (create_task, stop_task, list_tasks)
	// Required: true
	// Enum: create_task,stop_task,list_tasks
	// Example: create_task
	Action string `json:"action" example:"create_task"`

	// Payload de la acción
	// Example: {"name":"hello-world","image":"posts_mpv-remote-process","command":["echo","Hello, World!"],"env":{"GREETING":"Hello"},"working_dir":"/tmp","instance_type":"docker"}
	Payload json.RawMessage `json:"payload"`
}

// TaskRequest representa una solicitud de tarea
// swagger:model
type TaskRequest struct {
	// Nombre de la tarea
	// Required: true
	// Example: hello-world
	Name string `json:"name" example:"hello-world"`

	// Imagen Docker a usar
	// Required: true
	// Example: posts_mpv-remote-process
	Image string `json:"image" example:"posts_mpv-remote-process"`

	// Comando a ejecutar
	// Example: ["echo","Hello, World!"]
	Command []string `json:"command,omitempty" example:"[\"echo\",\"Hello, World!\"]"`

	// Variables de entorno
	// Example: {"GREETING":"Hello"}
	Env map[string]string `json:"env,omitempty" example:"{\"GREETING\":\"Hello\"}"`

	// Directorio de trabajo
	// Example: /tmp
	WorkingDir string `json:"working_dir,omitempty" example:"/tmp"`

	// Tipo de instancia (docker, kubernetes)
	// Example: docker
	InstanceType string `json:"instance_type,omitempty" example:"docker"`

	// Timeout en segundos
	// Example: 60
	Timeout int `json:"timeout,omitempty" example:"60"`
}

// TaskResponse representa la respuesta de una tarea
// swagger:model
type TaskResponse struct {
	// ID único de la tarea
	// Example: task-123
	TaskID string `json:"task_id" example:"task-123"`

	// Estado actual de la tarea (pending, running, completed, failed, stopped)
	// Example: completed
	Status string `json:"status" example:"completed"`

	// Salida de la tarea
	// Example: Hello, World!
	Output string `json:"output,omitempty" example:"Hello, World!"`

	// Error si ocurrió alguno
	// Example: 
	Error string `json:"error,omitempty"`

	// Indica si hubo error
	// Example: false
	IsError bool `json:"is_error" example:"false"`

	// Código de salida
	// Example: 0
	ExitCode string `json:"exit_code,omitempty" example:"0"`

	// Fecha de finalización
	// Example: 2025-01-24T19:06:51Z
	CompletedAt string `json:"completed_at,omitempty" example:"2025-01-24T19:06:51Z"`
}

// TaskListResponse representa la lista de tareas
// swagger:model
type TaskListResponse struct {
	// Lista de tareas
	Tasks []TaskResponse `json:"tasks"`
}

// HealthResponse representa la respuesta del health check
// swagger:model
type HealthResponse struct {
	// Estado del servicio
	// Example: healthy
	Status string `json:"status"`
}

// ErrorResponse representa un error en la API
// swagger:model
type ErrorResponse struct {
	// Código de error
	// Example: invalid_request
	Code string `json:"code"`

	// Mensaje de error
	// Example: Invalid request parameters
	Message string `json:"message"`
}
