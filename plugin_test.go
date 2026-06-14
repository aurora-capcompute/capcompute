package capcompute

import (
	"capcompute/dispatcher"
	"context"
	"testing"
)

type testSessionKey struct {
	id    string
	value string
}

func (k testSessionKey) SessionKey() string {
	return k.id
}

type testDispatcher struct{}

func (testDispatcher) Dispatch(context.Context, testSessionKey, dispatcher.Call) (dispatcher.Outcome, error) {
	return dispatcher.Result(nil), nil
}

type testDispatcherFactory struct {
	err error
}

func (f testDispatcherFactory) NewDispatcher(context.Context, testSessionKey) (dispatcher.Dispatcher[testSessionKey], error) {
	if f.err != nil {
		return nil, f.err
	}
	return testDispatcher{}, nil
}

type testSessionStore struct {
	sessions map[string]*Session[testSessionKey]
	active   map[string]struct{}
	saveErr  error
	endErr   error
}

func newTestSessionStore(sessions map[string]*Session[testSessionKey]) *testSessionStore {
	if sessions == nil {
		sessions = make(map[string]*Session[testSessionKey])
	}
	return &testSessionStore{sessions: sessions, active: make(map[string]struct{})}
}

func (s *testSessionStore) LoadSession(_ context.Context, sessionID string) (*Session[testSessionKey], error) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, ErrSessionRequired
	}
	return session, nil
}

func (s *testSessionStore) SaveSession(_ context.Context, sessionID string, session *Session[testSessionKey]) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.sessions[sessionID] = session
	return nil
}

func (s *testSessionStore) DeleteSession(_ context.Context, sessionID string) error {
	delete(s.sessions, sessionID)
	delete(s.active, sessionID)
	return nil
}

func (s *testSessionStore) ListSessions(context.Context) (map[string]*Session[testSessionKey], error) {
	sessions := make(map[string]*Session[testSessionKey], len(s.sessions))
	for sessionID, session := range s.sessions {
		sessions[sessionID] = session
	}
	return sessions, nil
}

func (s *testSessionStore) BeginSession(_ context.Context, sessionID string) error {
	if _, ok := s.sessions[sessionID]; !ok {
		return ErrSessionRequired
	}
	if _, ok := s.active[sessionID]; ok {
		return ErrSessionActive
	}
	s.active[sessionID] = struct{}{}
	return nil
}

func (s *testSessionStore) EndSession(_ context.Context, sessionID string) error {
	if s.endErr != nil {
		return s.endErr
	}
	delete(s.active, sessionID)
	return nil
}

func (s *testSessionStore) IsSessionActive(_ context.Context, sessionID string) (bool, error) {
	if _, ok := s.sessions[sessionID]; !ok {
		return false, ErrSessionRequired
	}
	_, ok := s.active[sessionID]
	return ok, nil
}

func (s *testSessionStore) markActive(sessionID string) {
	s.active[sessionID] = struct{}{}
}

func TestNewComputeCompiledPluginRequiresDispatcherFactory(t *testing.T) {
	_, err := NewComputeCompiledPlugin[string, testSessionKey](context.Background(), Config[string, testSessionKey]{
		SessionStore: newTestSessionStore(nil),
	})
	if err != ErrDispatcherRequired {
		t.Fatalf("error = %v, want ErrDispatcherRequired", err)
	}
}

func TestNewComputeCompiledPluginRequiresSessionStore(t *testing.T) {
	_, err := NewComputeCompiledPlugin[string, testSessionKey](context.Background(), Config[string, testSessionKey]{
		Dispatchers: testDispatcherFactory{},
	})
	if err != ErrSessionStoreRequired {
		t.Fatalf("error = %v, want ErrSessionStoreRequired", err)
	}
}

func TestPlayStatusReadsYieldedOutput(t *testing.T) {
	if got := playStatus([]byte(`{"status":"yielded"}`)); got != PlayYielded {
		t.Fatalf("status = %s, want %s", got, PlayYielded)
	}
}

func TestPlayStatusDefaultsToCompleted(t *testing.T) {
	for _, output := range [][]byte{
		[]byte(`{"status":"completed"}`),
		[]byte(`{"answer":"done"}`),
		[]byte(`not json`),
	} {
		if got := playStatus(output); got != PlayCompleted {
			t.Fatalf("status = %s for %s, want %s", got, output, PlayCompleted)
		}
	}
}
