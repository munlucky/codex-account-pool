package adminapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	stateVersion            = 1
	maxRetainedTerminalJobs = 256
)

type persistedState struct {
	Version int                    `json:"version"`
	Jobs    map[string]Job         `json:"jobs"`
	Checks  map[string]CheckRecord `json:"checks"`
}

type StateStore struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	state persistedState
}

func OpenStateStore(root string) (*StateStore, error) {
	s := &StateStore{
		path:  filepath.Join(root, "admin-state.json"),
		now:   time.Now,
		state: persistedState{Version: stateVersion, Jobs: map[string]Job{}, Checks: map[string]CheckRecord{}},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *StateStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read admin state: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return fmt.Errorf("decode admin state: %w", err)
	}
	if s.state.Version != stateVersion {
		return fmt.Errorf("unsupported admin state version %d", s.state.Version)
	}
	if s.state.Jobs == nil {
		s.state.Jobs = map[string]Job{}
	}
	if s.state.Checks == nil {
		s.state.Checks = map[string]CheckRecord{}
	}
	changed := false
	for id, job := range s.state.Jobs {
		if job.State == JobChecking || job.State == JobLoggingIn || job.State == JobFinalizing ||
			(job.Type == JobTypeLogin && job.State == JobQueued) {
			job.State = JobFailed
			job.ErrorCode = ErrServiceRestarted
			job.VerificationURL = ""
			job.UserCode = ""
			job.UpdatedAt = s.now().UTC()
			s.state.Jobs[id] = job
			changed = true
		}
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

func (s *StateStore) CreateJob(kind, provider, profileID string, newProfile bool) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	state := JobLoggingIn
	if kind == JobTypeCheck {
		state = JobChecking
	}
	job := Job{
		ID: randomID("j_"), Type: kind, Provider: provider, ProfileID: profileID,
		NewProfile: newProfile, State: state, CreatedAt: now, UpdatedAt: now,
	}
	s.state.Jobs[job.ID] = job
	return job, s.saveLocked()
}

func (s *StateStore) CreateLoginJob(provider, profileID string, newProfile bool) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.state.Jobs {
		active := existing.State == JobQueued || existing.State == JobChecking || existing.State == JobLoggingIn || existing.State == JobFinalizing
		if !active {
			continue
		}
		if existing.Provider == provider && existing.ProfileID == profileID {
			return Job{}, fmt.Errorf("profile operation already active: %s", existing.ID)
		}
		if existing.Type == JobTypeLogin {
			return Job{}, fmt.Errorf("another login operation is already active: %s", existing.ID)
		}
	}
	now := s.now().UTC()
	job := Job{
		ID: randomID("j_"), Type: JobTypeLogin, Provider: provider, ProfileID: profileID,
		NewProfile: newProfile, State: JobLoggingIn, CreatedAt: now, UpdatedAt: now,
	}
	s.state.Jobs[job.ID] = job
	return job, s.saveLocked()
}

func (s *StateStore) Job(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.state.Jobs[id]
	return job, ok
}

func (s *StateStore) ActiveJob(provider, profileID string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found Job
	for _, job := range s.state.Jobs {
		if job.Provider != provider || job.ProfileID != profileID {
			continue
		}
		if job.State != JobChecking && job.State != JobLoggingIn && job.State != JobFinalizing && job.State != JobQueued {
			continue
		}
		if found.ID == "" || job.CreatedAt.After(found.CreatedAt) {
			found = job
		}
	}
	return found, found.ID != ""
}

func (s *StateStore) ActiveJobs() []PublicJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]Job, 0)
	for _, job := range s.state.Jobs {
		if job.State == JobChecking || job.State == JobLoggingIn || job.State == JobFinalizing || job.State == JobQueued {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.Before(jobs[j].CreatedAt) })
	result := make([]PublicJob, 0, len(jobs))
	for _, job := range jobs {
		result = append(result, job.Public())
	}
	return result
}

func (s *StateStore) RecentJob(kind, provider, profileID string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found Job
	for _, job := range s.state.Jobs {
		if job.Type != kind || job.Provider != provider || job.ProfileID != profileID {
			continue
		}
		if found.ID == "" || job.CreatedAt.After(found.CreatedAt) {
			found = job
		}
	}
	return found, found.ID != ""
}

func (s *StateStore) CompleteCheck(id string, record CheckRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.state.Jobs[id]
	if !ok {
		return errors.New("job not found")
	}
	if job.State == JobCanceled {
		return nil
	}
	job.UpdatedAt = s.now().UTC()
	if record.Status == StatusConnected {
		job.State = JobSucceeded
		job.ErrorCode = ""
	} else {
		job.State = JobFailed
		job.ErrorCode = record.ErrorCode
	}
	s.state.Jobs[id] = job
	s.state.Checks[profileKey(job.Provider, job.ProfileID)] = record
	return s.saveLocked()
}

func (s *StateStore) RecordCheck(provider, profileID string, record CheckRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Checks[profileKey(provider, profileID)] = record
	return s.saveLocked()
}

func (s *StateStore) Check(provider, profileID string) (CheckRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Checks[profileKey(provider, profileID)]
	return record, ok
}

func (s *StateStore) SetLoginChallenge(id, verificationURL, userCode string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.state.Jobs[id]
	if !ok {
		return Job{}, errors.New("job not found")
	}
	if job.Type != JobTypeLogin || job.State != JobLoggingIn {
		return job, errors.New("login job is not active")
	}
	job.VerificationURL = verificationURL
	job.UserCode = userCode
	job.UpdatedAt = s.now().UTC()
	s.state.Jobs[id] = job
	return job, s.saveLocked()
}

func (s *StateStore) BeginLoginFinalization(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.state.Jobs[id]
	if !ok {
		return Job{}, errors.New("job not found")
	}
	if job.Type != JobTypeLogin || job.State != JobLoggingIn {
		return job, errors.New("login job is not active")
	}
	job.State = JobFinalizing
	job.VerificationURL = ""
	job.UserCode = ""
	job.UpdatedAt = s.now().UTC()
	s.state.Jobs[id] = job
	return job, s.saveLocked()
}

func (s *StateStore) FinishLogin(id string, success bool, errorCode string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.state.Jobs[id]
	if !ok {
		return Job{}, errors.New("job not found")
	}
	if job.Type != JobTypeLogin || job.State != JobFinalizing {
		return job, errors.New("login finalization is not active")
	}
	job.UpdatedAt = s.now().UTC()
	job.VerificationURL = ""
	job.UserCode = ""
	if success {
		job.State = JobSucceeded
		job.ErrorCode = ""
	} else {
		job.State = JobFailed
		job.ErrorCode = errorCode
	}
	s.state.Jobs[id] = job
	return job, s.saveLocked()
}

func (s *StateStore) CancelJob(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.state.Jobs[id]
	if !ok {
		return Job{}, errors.New("job not found")
	}
	if job.State == JobSucceeded || job.State == JobFailed || job.State == JobCanceled {
		return job, nil
	}
	if job.State == JobFinalizing {
		return job, errors.New("job finalization already started")
	}
	job.State = JobCanceled
	job.ErrorCode = ErrCanceled
	job.VerificationURL = ""
	job.UserCode = ""
	job.UpdatedAt = s.now().UTC()
	s.state.Jobs[id] = job
	return job, s.saveLocked()
}

func (s *StateStore) FailJob(id, errorCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.state.Jobs[id]
	if !ok {
		return errors.New("job not found")
	}
	if job.State == JobSucceeded || job.State == JobCanceled {
		return nil
	}
	job.State = JobFailed
	job.ErrorCode = errorCode
	job.VerificationURL = ""
	job.UserCode = ""
	job.UpdatedAt = s.now().UTC()
	s.state.Jobs[id] = job
	return s.saveLocked()
}

func (s *StateStore) pruneJobsLocked() {
	var terminal []Job
	for _, job := range s.state.Jobs {
		switch job.State {
		case JobSucceeded, JobFailed, JobCanceled:
			terminal = append(terminal, job)
		}
	}
	if len(terminal) <= maxRetainedTerminalJobs {
		return
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].UpdatedAt.Equal(terminal[j].UpdatedAt) {
			return terminal[i].CreatedAt.Before(terminal[j].CreatedAt)
		}
		return terminal[i].UpdatedAt.Before(terminal[j].UpdatedAt)
	})
	for _, job := range terminal[:len(terminal)-maxRetainedTerminalJobs] {
		delete(s.state.Jobs, job.ID)
	}
}

func (s *StateStore) saveLocked() error {
	s.pruneJobsLocked()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create admin state directory: %w", err)
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode admin state: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".admin-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create admin state temp file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("replace admin state: %w", err)
	}
	_ = os.Chmod(s.path, 0o600)
	return nil
}

func profileKey(provider, profileID string) string { return provider + "/" + profileID }

func randomID(prefix string) string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(raw[:])
}
