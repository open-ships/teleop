package teleop

import "strconv"

// String returns a stable human-readable event identity. Stream is quoted so
// application-defined separators cannot make two identities ambiguous.
func (id EventID) String() string {
	return id.Session.String() + "/" + strconv.Quote(string(id.Stream)) + "/" +
		strconv.FormatUint(id.Sequence, 10)
}
