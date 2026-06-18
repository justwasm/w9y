package w9y

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type BuildJob struct {
	ID        string    `json:"id"`
	Spec      string    `json:"spec"`
	Runtime   string    `json:"runtime"`
	Status    string    `json:"status"` // pending, building, done, error
	Result    string    `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
}

type JobStore struct {
	mu   sync.Mutex
	jobs map[string]*BuildJob
}

func NewJobStore() *JobStore {
	return &JobStore{jobs: make(map[string]*BuildJob)}
}

func (s *JobStore) Create(spec, runtime string) *BuildJob {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := generateID()
	job := &BuildJob{
		ID:        id,
		Spec:      spec,
		Runtime:   runtime,
		Status:    "pending",
		CreatedAt: time.Now(),
	}
	s.jobs[id] = job
	return job
}

func (s *JobStore) Get(id string) (*BuildJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *JobStore) SetBuilding(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.jobs[id]; ok {
		job.Status = "building"
	}
}

func (s *JobStore) SetDone(id, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.jobs[id]; ok {
		now := time.Now()
		job.Status = "done"
		job.Result = result
		job.DoneAt = &now
	}
}

func (s *JobStore) SetError(id, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job, ok := s.jobs[id]; ok {
		now := time.Now()
		job.Status = "error"
		job.Error = errMsg
		job.DoneAt = &now
	}
}

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
