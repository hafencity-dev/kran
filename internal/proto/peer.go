package proto

// Peer protocol (SPEC §8.3). Every yamux stream starts with one JSON line:
// a PeerRequest. The responder answers with one JSON line PeerResponse, after
// which the stream carries method-specific data (documented per method in
// internal/transport and internal/daemon). The details live there; the
// types here are the stable envelope.

type PeerRequest struct {
	Method string `json:"method"`
	JobID  string `json:"job_id,omitempty"`
	Body   any    `json:"body,omitempty"`
}

type PeerResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"` // machine-readable error code, e.g. "disk_floor", "slot_busy", "version"
	Body  any    `json:"body,omitempty"`
}

type Hello struct {
	Node     NodeInfo `json:"node"`
	Protocol string   `json:"protocol"` // "major.minor"
	Peers    []string `json:"peers,omitempty"`
}

// Peer method names.
const (
	PeerHello            = "hello"
	PeerMetricsSubscribe = "metrics.subscribe"
	PeerSlotAcquire      = "slot.acquire"
	PeerSlotDiff         = "slot.diff"
	PeerBlobPut          = "blob.put"
	PeerSlotCommit       = "slot.commit"
	PeerSlotRelease      = "slot.release"
	PeerJobStart         = "job.start"
	PeerJobAttach        = "job.attach"
	PeerJobHeartbeat     = "job.heartbeat"
	PeerJobSignal        = "job.signal"
	PeerJobResize        = "job.resize"
	PeerJobCancel        = "job.cancel"
	PeerMeshPeers        = "mesh.peers"
	PeerAdmit            = "admit" // ask target to reserve resources for a job (two-phase admission)
	// the peer's current time (body {"time"}), to measure the
	// clock offset over the live session.
	PeerClock = "clock"
)
