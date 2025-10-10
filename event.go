package whisper

type (
	EventType int

	Event struct {
		Type EventType
		Peer Peer
	}
)

const (
	EventTypeUnknown EventType = iota
	EventTypePeerDiscovered
	EventTypePeerUpdated
)
