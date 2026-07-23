package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a store backed by a throwaway file.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sample(name string) Database {
	return Database{
		Name:          name,
		Engine:        "postgres",
		ContainerID:   "container-id-" + name,
		ContainerName: "sparkdb-postgres-" + name,
		VolumeName:    "sparkdb-postgres-" + name + "-data",
		Username:      "haki",
		Password:      "secret",
		Status:        StatusProvisioning,
		CreatedAt:     time.Now(),
	}
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	want := sample("mydb")
	if err := st.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := st.Get(ctx, "mydb")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != want.Name || got.Engine != want.Engine {
		t.Errorf("identity mismatch: got %+v", got)
	}
	if got.ContainerName != want.ContainerName || got.VolumeName != want.VolumeName {
		t.Errorf("compute metadata mismatch: got %+v", got)
	}
	if got.Username != want.Username || got.Password != want.Password {
		t.Errorf("credentials mismatch: got %+v", got)
	}
	if got.Status != StatusProvisioning {
		t.Errorf("Status = %q, want %q", got.Status, StatusProvisioning)
	}
	if got.CreatedAt.Unix() != want.CreatedAt.Unix() {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.Create(ctx, sample("mydb")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := st.Create(ctx, sample("mydb"))
	if !errors.Is(err, ErrExists) {
		t.Errorf("err = %v, want ErrExists", err)
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	st := newTestStore(t)
	_, err := st.Get(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSetStatus(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.Create(ctx, sample("mydb")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := st.SetStatus(ctx, "mydb", StatusRunning); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, err := st.Get(ctx, "mydb")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}
}

func TestSetContainer(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	rec := sample("mydb")
	rec.ContainerID = ""
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := st.SetContainer(ctx, "mydb", "abc123"); err != nil {
		t.Fatalf("SetContainer: %v", err)
	}
	got, _ := st.Get(ctx, "mydb")
	if got.ContainerID != "abc123" {
		t.Errorf("ContainerID = %q, want %q", got.ContainerID, "abc123")
	}
}

func TestTouchPersistsLastActive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.Create(ctx, sample("mydb")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	when := time.Now().Add(-30 * time.Second)
	if err := st.Touch(ctx, "mydb", when); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, _ := st.Get(ctx, "mydb")
	if got.LastActive.Unix() != when.Unix() {
		t.Errorf("LastActive = %v, want %v", got.LastActive, when)
	}
}

func TestMutationsOnMissingRowReturnNotFound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	tests := map[string]error{
		"SetStatus":    st.SetStatus(ctx, "ghost", StatusRunning),
		"SetContainer": st.SetContainer(ctx, "ghost", "abc"),
		"Touch":        st.Touch(ctx, "ghost", time.Now()),
		"Delete":       st.Delete(ctx, "ghost"),
	}
	for name, err := range tests {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestListReturnsOldestFirst(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	old := sample("older")
	old.CreatedAt = time.Now().Add(-time.Hour)
	recent := sample("newer")
	recent.CreatedAt = time.Now()

	if err := st.Create(ctx, recent); err != nil {
		t.Fatalf("Create newer: %v", err)
	}
	if err := st.Create(ctx, old); err != nil {
		t.Fatalf("Create older: %v", err)
	}

	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}
	if list[0].Name != "older" || list[1].Name != "newer" {
		t.Errorf("order = [%s %s], want [older newer]", list[0].Name, list[1].Name)
	}
}

func TestListEmpty(t *testing.T) {
	st := newTestStore(t)
	list, err := st.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("len(list) = %d, want 0", len(list))
	}
}

func TestDeleteRemovesRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.Create(ctx, sample("mydb")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := st.Delete(ctx, "mydb"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, "mydb"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound after delete", err)
	}
}

func TestStateSurvivesReopen(t *testing.T) {
	// The manager hydrates from this file at boot, so persistence matters.
	path := filepath.Join(t.TempDir(), "persist.db")
	ctx := context.Background()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Create(ctx, sample("mydb")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.SetStatus(ctx, "mydb", StatusStopped); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	st.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.Get(ctx, "mydb")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Status != StatusStopped {
		t.Errorf("Status = %q, want %q", got.Status, StatusStopped)
	}
}
