package mem

import "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"

const AcceptQueueSize = acceptQueueSize

// OpenStreams reports how many stream slots of the connection are taken.
func OpenStreams(c transport.Conn) int { return len(c.(*conn).s.slots) }
