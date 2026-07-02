package cdi

// Handler generates the node CDI specification and builds the CDI references
// (annotations / CDI devices) that are returned to kubelet during Allocate.
type Handler interface {
	// CreateSpecFile generates the CDI specification file describing the node's
	// devices. It is invoked once at plugin startup.
	CreateSpecFile() error
	// QualifiedName returns the fully-qualified CDI device name for the given
	// class and device id (e.g. "k8s.device-plugin.nvidia.com/gpu=<uuid>").
	QualifiedName(class, id string) string
	// GetDeviceAnnotations builds the CDI container annotations for the given
	// qualified device names, honoring the configured annotation prefix.
	GetDeviceAnnotations(responseID string, qualifiedNames []string) (map[string]string, error)
}

// null is a no-op Handler used when no CDI strategy is enabled.
type null struct{}

// NewNullHandler returns a Handler that performs no CDI operations.
func NewNullHandler() Handler {
	return &null{}
}

func (n *null) CreateSpecFile() error {
	return nil
}

func (n *null) QualifiedName(_, _ string) string {
	return ""
}

func (n *null) GetDeviceAnnotations(_ string, _ []string) (map[string]string, error) {
	return nil, nil
}
