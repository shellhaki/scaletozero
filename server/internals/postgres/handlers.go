package postgres

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handler holds the dependencies for the Postgres HTTP endpoints.
type Handler struct {
	Client *Client
}

// NewHandler wires a Handler to a Client.
func NewHandler(cl *Client) *Handler {
	return &Handler{Client: cl}
}

// Register mounts all Postgres routes on the given router.
func (h *Handler) Register(r gin.IRoutes) {
	r.POST("/api/postgres/create", h.Create)
	r.POST("/api/postgres/start", h.Start)
	r.POST("/api/postgres/stop", h.Stop)
	r.POST("/api/postgres/pause", h.Pause)
	r.POST("/api/postgres/delete", h.Delete)
}

func (h *Handler) Create(c *gin.Context) {
	var req CreatePostgresRequest
	if !bind(c, &req) {
		return
	}
	port, err := h.Client.Create(req.Username, req.Password, req.Name)
	if err != nil {
		fail(c, "error while creating container", err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"message": "Postgres container created successfully",
		"name":    req.Name,
		"port":    port,
	})
}

func (h *Handler) Start(c *gin.Context) {
	var req StartPostgresRequest
	if !bind(c, &req) {
		return
	}
	if err := h.Client.Start(req.Name); err != nil {
		fail(c, "error while starting container", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Postgres started successfully", "name": req.Name})
}

func (h *Handler) Stop(c *gin.Context) {
	var req StopPostgresRequest
	if !bind(c, &req) {
		return
	}
	if err := h.Client.Stop(req.Name); err != nil {
		fail(c, "error while stopping container", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Postgres stopped successfully", "name": req.Name})
}

func (h *Handler) Pause(c *gin.Context) {
	var req PausePostgresRequest
	if !bind(c, &req) {
		return
	}
	if err := h.Client.Pause(req.Name); err != nil {
		fail(c, "error while pausing container", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Postgres paused successfully", "name": req.Name})
}

func (h *Handler) Delete(c *gin.Context) {
	var req DeletePostgresRequest
	if !bind(c, &req) {
		return
	}
	if err := h.Client.Delete(req.Name); err != nil {
		fail(c, "error while deleting container", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Postgres deleted successfully", "name": req.Name})
}

// bind decodes the JSON body into dst. On failure it writes a 400 and returns
// false so the caller can stop.
func bind(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"message": "invalid request", "error": err.Error()})
		return false
	}
	return true
}

// fail writes a 500 with the given message and error.
func fail(c *gin.Context, msg string, err error) {
	c.JSON(http.StatusInternalServerError, gin.H{"message": msg, "error": err.Error()})
}