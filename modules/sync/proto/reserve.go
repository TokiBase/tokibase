package proto

// Routes of PR8 (docs/SYNC_DESIGN.md §3.10).
const (
	PathReserve        = "/api/sync/reserve"
	PathReserveRelease = "/api/sync/reserve/release"
)

// CodeReservationLimit is the 429 code of a refused reservation (more open
// ranges than max_open_per_node, or a count above max_block).
const (
	CodeReservationLimit      = "sync_reservation_limit"
	CodeReservationExhausted  = "sync_reservation_exhausted"
	CodeReservationOutOfRange = "reservation_out_of_range"
)

// Reservation is one range in the handshake `reservations` list.
type Reservation struct {
	ID            string `json:"id,omitempty"`
	Sequence      string `json:"sequence"`
	Start         int64  `json:"start"`
	End           int64  `json:"end"`
	RemainingHint int64  `json:"remaining_hint"`
	Expires       string `json:"expires,omitempty"`
}

// ReserveRequest is the body of POST /api/sync/reserve. Count 0 means the
// block size of the sequence. Used maps a range id to the highest value the
// node used from it (the hub treats it like a high-water mark, so a node that
// burned its range can get a new one before its pushes arrived).
type ReserveRequest struct {
	Sequence string           `json:"sequence"`
	Count    int64            `json:"count,omitempty"`
	Used     map[string]int64 `json:"used,omitempty"`
}

// ReserveRange is a range of a reserve answer.
type ReserveRange struct {
	ID      string `json:"id"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	Expires string `json:"expires"`
}

// ReserveResponse is the 200 body of POST /api/sync/reserve.
type ReserveResponse struct {
	Sequence string         `json:"sequence"`
	Ranges   []ReserveRange `json:"ranges"`
	Format   string         `json:"format"`
}

// ReserveList is the 200 body of GET /api/sync/reserve.
type ReserveList struct {
	Ranges []Reservation `json:"ranges"`
}

// ReleaseRequest is the body of POST /api/sync/reserve/release. Used is the
// highest value the node used from the range (0 = none).
type ReleaseRequest struct {
	ID   string `json:"id"`
	Used int64  `json:"used,omitempty"`
}
