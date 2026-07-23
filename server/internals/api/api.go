package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"sparkdb/scaletozero/internals/engine"
	"sparkdb/scaletozero/internals/manager"
	"sparkdb/scaletozero/internals/store"
)

// API exposes the control plane over HTTP.
type API struct {
	Manager *manager.Manager
	Engines engine.Registry
}

// New builds an API.
func New(mgr *manager.Manager, engines engine.Registry) *API {
	return &API{Manager: mgr, Engines: engines}
}

// CreateRequest is the body of a create call.
type CreateRequest struct {
	Name     string `json:"name" binding:"required"`
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// NameRequest is the body of the lifecycle calls.
type NameRequest struct {
	Name string `json:"name" binding:"required"`
}

// DatabaseResponse is the API view of a provisioned database. The stored
// password is deliberately omitted.
type DatabaseResponse struct {
	Name       string    `json:"name"`
	Engine     string    `json:"engine"`
	Status     string    `json:"status"`
	Username   string    `json:"username"`
	Container  string    `json:"container"`
	Volume     string    `json:"volume"`
	LastActive time.Time `json:"last_active"`
	CreatedAt  time.Time `json:"created_at"`
}

func view(d store.Database) DatabaseResponse {
	return DatabaseResponse{
		Name:       d.Name,
		Engine:     d.Engine,
		Status:     string(d.Status),
		Username:   d.Username,
		Container:  d.ContainerName,
		Volume:     d.VolumeName,
		LastActive: d.LastActive,
		CreatedAt:  d.CreatedAt,
	}
}

// Register mounts the control-plane routes. Lifecycle paths are engine scoped,
// so a new engine is reachable the moment it is registered.
func (a *API) Register(r gin.IRouter) {
	r.GET("/", a.Root)
	r.GET("/health", a.Health)
	r.GET("/api/databases", a.List)

	grp := r.Group("/api/:engine")
	grp.POST("/create", a.Create)
	grp.POST("/start", a.Start)
	grp.POST("/stop", a.Stop)
	grp.POST("/pause", a.Pause)
	grp.POST("/delete", a.Delete)
	grp.GET("/list", a.List)
}

func (a *API) Root(c *gin.Context) {
	engines := make([]string, 0, len(a.Engines))
	for name := range a.Engines {
		engines = append(engines, name)
	}
	c.JSON(http.StatusOK, gin.H{"message": "sparkdb scale-to-zero api", "engines": engines})
}

func (a *API) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (a *API) List(c *gin.Context) {
	all := a.Manager.List()
	wanted := c.Param("engine")

	out := make([]DatabaseResponse, 0, len(all))
	for _, d := range all {
		if wanted != "" && d.Engine != wanted {
			continue
		}
		out = append(out, view(d))
	}
	c.JSON(http.StatusOK, gin.H{"databases": out})
}

func (a *API) Create(c *gin.Context) {
	var req CreateRequest
	if !bind(c, &req) {
		return
	}
	db, err := a.Manager.Create(c.Request.Context(), c.Param("engine"), req.Name, req.Username, req.Password)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"message":  "database provisioned and scaled to zero",
		"database": view(*db),
	})
}

func (a *API) Start(c *gin.Context) {
	a.lifecycle(c, a.Manager.Start, "database started")
}

func (a *API) Stop(c *gin.Context) {
	a.lifecycle(c, a.Manager.Stop, "database scaled to zero")
}

func (a *API) Pause(c *gin.Context) {
	a.lifecycle(c, a.Manager.Pause, "database paused")
}

func (a *API) Delete(c *gin.Context) {
	a.lifecycle(c, a.Manager.Delete, "database and volume deleted")
}

// lifecycle runs a name-only manager action and renders the result.
func (a *API) lifecycle(c *gin.Context, action func(ctx context.Context, name string) error, msg string) {
	var req NameRequest
	if !bind(c, &req) {
		return
	}
	if err := action(c.Request.Context(), req.Name); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": msg, "name": req.Name})
}

func bind(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"message": "invalid request", "error": err.Error()})
		return false
	}
	return true
}

// fail maps domain errors onto status codes.
func fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"message": "database not found", "error": err.Error()})
	case errors.Is(err, store.ErrExists):
		c.JSON(http.StatusConflict, gin.H{"message": "database already exists", "error": err.Error()})
	case errors.Is(err, manager.ErrInvalidName):
		c.JSON(http.StatusBadRequest, gin.H{"message": "invalid database name", "error": err.Error()})
	case errors.Is(err, manager.ErrUnknownEngine):
		c.JSON(http.StatusBadRequest, gin.H{"message": "unknown engine", "error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"message": "operation failed", "error": err.Error()})
	}
}
