package analyzer

const (
	fingerprintMaxBytes = 2048
	fnvOffset           = 14695981039346656037
	fnvPrime            = 1099511628211
)

// ErrorFingerprint returns a non-zero 64-bit hash of the normalized start of a tool error
// output, or 0 when there is nothing to hash. raw may be the output's JSON encoding (as in the
// transcript): the escapes \n \r \t count as whitespace, \uXXXX is dropped and any other
// backslash is dropped. Normalization: ASCII letters lowercased, digits dropped (line
// numbers, durations, ids), whitespace runs collapsed and trimmed. Only the first
// fingerprintMaxBytes normalized bytes count. The input is read, never copied or kept; the
// hash stays on the machine (it is never part of the ingest payload).
func ErrorFingerprint(raw []byte) uint64 {
	h := uint64(fnvOffset)
	n := 0
	pendingSpace := false
	for i := 0; i < len(raw) && n < fingerprintMaxBytes; i++ {
		c := raw[i]
		if c == '\\' && i+1 < len(raw) {
			i++
			switch c = raw[i]; c {
			case 'n', 'r', 't':
				c = ' '
			case 'u':
				i = min(i+4, len(raw)-1) // skip the four hex digits
				continue
			}
		}
		switch {
		case c >= '0' && c <= '9':
			continue
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			pendingSpace = n > 0
			continue
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		}
		if pendingSpace {
			h = (h ^ ' ') * fnvPrime
			n++
			pendingSpace = false
		}
		h = (h ^ uint64(c)) * fnvPrime
		n++
	}
	if n == 0 {
		return 0
	}
	if h == 0 {
		h = 1
	}
	return h
}
