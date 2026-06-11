package forward

// Service is the public lifecycle wrapper for the forward Manager.
type Service struct {
	manager *Manager
}

// NewService creates a Service. Returns nil when forward is disabled.
func NewService(config *Config) *Service {
	if config == nil {
		config = DefaultConfig()
	}
	if !config.Enabled {
		return nil
	}
	manager := NewManager(config)
	if manager == nil {
		return nil
	}
	return &Service{manager: manager}
}

func (s *Service) Start() error {
	if s == nil {
		return nil
	}
	return s.manager.Start()
}

func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.manager.Stop()
}

func (s *Service) TryForwardRawTx(params []byte, txSource string) {
	if s == nil {
		return
	}
	s.manager.TryForwardRawTx(params, txSource)
}

func (s *Service) TryForwardBundle(params []byte, submittedFromDomain, clientIP string) {
	if s == nil {
		return
	}
	s.manager.TryForwardBundle(params, submittedFromDomain, clientIP)
}
