package postgres

type CreatePostgresRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	Name     string `json:"name" binding:"required"`
}

type StartPostgresRequest struct {
	Name string `json:"name" binding:"required"`
}

type StopPostgresRequest struct {
	Name string `json:"name" binding:"required"`
}

type PausePostgresRequest struct {
	Name string `json:"name" binding:"required"`
}

type DeletePostgresRequest struct {
	Name string `json:"name" binding:"required"`
}