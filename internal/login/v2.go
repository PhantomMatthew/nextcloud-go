package login

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	TokenLength    = 128
	TokenAlphabet  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	StateTokenLen  = 32
	DefaultTTL     = 20 * time.Minute
	DefaultGCEvery = 1 * time.Minute
)

var (
	ErrFlowNotFound = errors.New("login: flow not found")
	ErrFlowExpired  = errors.New("login: flow expired")
	ErrFlowPending  = errors.New("login: flow pending")
	ErrFlowConsumed = errors.New("login: flow consumed")
	ErrInvalidState = errors.New("login: invalid state token")
)

type FlowState int

const (
	StatePending FlowState = iota
	StateGranted
	StateConsumed
)

type Flow struct {
	PollToken   string
	LoginToken  string
	ClientName  string
	State       FlowState
	StateToken  string
	Server      string
	LoginName   string
	AppPassword string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type Store interface {
	Insert(ctx context.Context, f *Flow) error
	GetByPoll(ctx context.Context, pollToken string) (*Flow, error)
	GetByLogin(ctx context.Context, loginToken string) (*Flow, error)
	GetByState(ctx context.Context, stateToken string) (*Flow, error)
	Update(ctx context.Context, f *Flow) error
	DeleteExpired(ctx context.Context, now time.Time) int
}

type MemoryStore struct {
	mu       sync.RWMutex
	byPoll   map[string]*Flow
	byLogin  map[string]*Flow
	byState  map[string]*Flow
	stopOnce sync.Once
	stop     chan struct{}
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byPoll:  make(map[string]*Flow),
		byLogin: make(map[string]*Flow),
		byState: make(map[string]*Flow),
		stop:    make(chan struct{}),
	}
}

func (s *MemoryStore) StartGC(every time.Duration) {
	if every <= 0 {
		every = DefaultGCEvery
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case now := <-t.C:
				s.DeleteExpired(context.Background(), now.UTC())
			}
		}
	}()
}

func (s *MemoryStore) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *MemoryStore) Insert(_ context.Context, f *Flow) error {
	if f == nil || f.PollToken == "" || f.LoginToken == "" {
		return errors.New("login: invalid flow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *f
	s.byPoll[f.PollToken] = &cp
	s.byLogin[f.LoginToken] = &cp
	return nil
}

func (s *MemoryStore) GetByPoll(_ context.Context, pollToken string) (*Flow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.byPoll[pollToken]
	if !ok {
		return nil, ErrFlowNotFound
	}
	cp := *f
	return &cp, nil
}

func (s *MemoryStore) GetByLogin(_ context.Context, loginToken string) (*Flow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.byLogin[loginToken]
	if !ok {
		return nil, ErrFlowNotFound
	}
	cp := *f
	return &cp, nil
}

func (s *MemoryStore) GetByState(_ context.Context, stateToken string) (*Flow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.byState[stateToken]
	if !ok {
		return nil, ErrFlowNotFound
	}
	cp := *f
	return &cp, nil
}

func (s *MemoryStore) Update(_ context.Context, f *Flow) error {
	if f == nil || f.PollToken == "" {
		return errors.New("login: invalid flow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byPoll[f.PollToken]
	if !ok {
		return ErrFlowNotFound
	}
	if cur.StateToken != "" && cur.StateToken != f.StateToken {
		delete(s.byState, cur.StateToken)
	}
	cp := *f
	s.byPoll[f.PollToken] = &cp
	s.byLogin[f.LoginToken] = &cp
	if f.StateToken != "" {
		s.byState[f.StateToken] = &cp
	}
	return nil
}

func (s *MemoryStore) DeleteExpired(_ context.Context, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for tok, f := range s.byPoll {
		if !f.ExpiresAt.IsZero() && now.After(f.ExpiresAt) {
			delete(s.byPoll, tok)
			delete(s.byLogin, f.LoginToken)
			if f.StateToken != "" {
				delete(s.byState, f.StateToken)
			}
			n++
		}
	}
	return n
}

type Service struct {
	store Store
	ttl   time.Duration
	now   func() time.Time
}

func NewService(store Store) *Service {
	return &Service{store: store, ttl: DefaultTTL, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) WithTTL(ttl time.Duration) *Service { s.ttl = ttl; return s }
func (s *Service) WithClock(fn func() time.Time) *Service {
	s.now = fn
	return s
}

func (s *Service) Init(ctx context.Context, clientName string) (*Flow, error) {
	poll, err := generateAlphaToken(TokenLength)
	if err != nil {
		return nil, fmt.Errorf("login: generate poll token: %w", err)
	}
	login, err := generateAlphaToken(TokenLength)
	if err != nil {
		return nil, fmt.Errorf("login: generate login token: %w", err)
	}
	now := s.now()
	f := &Flow{
		PollToken:  poll,
		LoginToken: login,
		ClientName: clientName,
		State:      StatePending,
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.ttl),
	}
	if err := s.store.Insert(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Service) Poll(ctx context.Context, pollToken string) (*Flow, error) {
	f, err := s.store.GetByPoll(ctx, pollToken)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !f.ExpiresAt.IsZero() && now.After(f.ExpiresAt) {
		return nil, ErrFlowExpired
	}
	switch f.State {
	case StatePending:
		return nil, ErrFlowPending
	case StateConsumed:
		return nil, ErrFlowConsumed
	case StateGranted:
		f.State = StateConsumed
		if err := s.store.Update(ctx, f); err != nil {
			return nil, err
		}
		return f, nil
	}
	return nil, ErrFlowNotFound
}

func (s *Service) BeginGrant(ctx context.Context, loginToken string) (*Flow, error) {
	f, err := s.store.GetByLogin(ctx, loginToken)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !f.ExpiresAt.IsZero() && now.After(f.ExpiresAt) {
		return nil, ErrFlowExpired
	}
	if f.State != StatePending {
		return nil, ErrFlowConsumed
	}
	st, err := generateHexToken(StateTokenLen)
	if err != nil {
		return nil, fmt.Errorf("login: generate state token: %w", err)
	}
	f.StateToken = st
	if err := s.store.Update(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Service) LookupState(ctx context.Context, stateToken string) (*Flow, error) {
	if stateToken == "" {
		return nil, ErrInvalidState
	}
	f, err := s.store.GetByState(ctx, stateToken)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if !f.ExpiresAt.IsZero() && now.After(f.ExpiresAt) {
		return nil, ErrFlowExpired
	}
	if f.State != StatePending {
		return nil, ErrFlowConsumed
	}
	return f, nil
}

func (s *Service) Grant(ctx context.Context, stateToken, server, loginName, appPassword string) (*Flow, error) {
	f, err := s.LookupState(ctx, stateToken)
	if err != nil {
		return nil, err
	}
	f.State = StateGranted
	f.Server = server
	f.LoginName = loginName
	f.AppPassword = appPassword
	f.StateToken = ""
	if err := s.store.Update(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

func generateAlphaToken(n int) (string, error) {
	if n <= 0 {
		return "", errors.New("login: token length must be positive")
	}
	out := make([]byte, n)
	alpha := []byte(TokenAlphabet)
	alphaLen := byte(len(alpha))
	max := byte(256 - (256 % int(alphaLen)))
	buf := make([]byte, n*2)
	filled := 0
	for filled < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("login: read random: %w", err)
		}
		for _, b := range buf {
			if b >= max {
				continue
			}
			out[filled] = alpha[b%alphaLen]
			filled++
			if filled == n {
				break
			}
		}
	}
	return string(out), nil
}

func generateHexToken(nBytes int) (string, error) {
	if nBytes <= 0 {
		return "", errors.New("login: hex length must be positive")
	}
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("login: read random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
