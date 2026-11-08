package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Decode errors.
var (
	// ErrUnsupportedProtocol: the envelope's v is above ProtocolV1; the server answers the whole
	// request 400 unsupported_protocol.
	ErrUnsupportedProtocol = errors.New("wire: unsupported protocol version")
	// ErrSchema: the envelope or body fails v1 validation; the server answers that seq rejected/schema.
	ErrSchema = errors.New("wire: schema violation")
)

// DecodeEnvelope is the protocol v1 envelope decoder, kept forever. It parses raw
// (one CAB1 type-1 record) and checks: v (above ProtocolV1 wraps ErrUnsupportedProtocol, below 1 wraps
// ErrSchema), a known Kind, ValidKey, seq >= 1, outbox_id of 32 lowercase hex, Loc present exactly for
// import kinds, and a JSON object Body. Any of those failing wraps ErrSchema. It does not judge
// mask_version or canon_version (server policy) and does not decode Body. Unknown fields are
// ignored here; the stored body keeps them.
//
// v is read first, on its own, so an envelope of a later protocol is ErrUnsupportedProtocol even
// when the rest of it does not fit the v1 struct. The returned Body is the record's body bytes
// exactly as sent (body_sha256 is computed over them).
func DecodeEnvelope(raw []byte) (Envelope, error) {
	var head struct {
		V json.Number `json:"v"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Envelope{}, fmt.Errorf("%w: envelope: %v", ErrSchema, err)
	}
	if err := checkVersion(head.V); err != nil {
		return Envelope{}, err
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return Envelope{}, fmt.Errorf("%w: envelope: %v", ErrSchema, err)
	}
	switch {
	case !e.Kind.known():
		return Envelope{}, fmt.Errorf("%w: unknown kind %q", ErrSchema, e.Kind)
	case !ValidKey(e.Key):
		return Envelope{}, fmt.Errorf("%w: invalid key", ErrSchema)
	case e.Seq < 1:
		return Envelope{}, fmt.Errorf("%w: seq must be >= 1", ErrSchema)
	case !validOutboxID(e.OutboxID):
		return Envelope{}, fmt.Errorf("%w: outbox_id must be 32 lowercase hex", ErrSchema)
	case e.Kind.SourceScoped() && e.Loc != nil:
		return Envelope{}, fmt.Errorf("%w: loc on a %s envelope", ErrSchema, e.Kind)
	case !e.Kind.SourceScoped() && e.Loc == nil:
		return Envelope{}, fmt.Errorf("%w: %s envelope without loc", ErrSchema, e.Kind)
	case !isJSONObject(e.Body):
		return Envelope{}, fmt.Errorf("%w: body must be a JSON object", ErrSchema)
	}
	return e, nil
}

// DecodeBody decodes an envelope body for kind into a pointer to its struct: *ExchangeBegin,
// *ExchangeAttempt, *ExchangeResponse, *ExchangeCompletion, *UsageAttempt, *ImportRecord or
// *ImportFile. A body that is not valid JSON for its struct, or an unknown kind, wraps ErrSchema.
func DecodeBody(kind Kind, body json.RawMessage) (any, error) {
	var v any
	switch kind {
	case KindExchangeBegin:
		v = new(ExchangeBegin)
	case KindExchangeAttempt:
		v = new(ExchangeAttempt)
	case KindExchangeResponse:
		v = new(ExchangeResponse)
	case KindExchangeDone:
		v = new(ExchangeCompletion)
	case KindUsage:
		v = new(UsageAttempt)
	case KindImportRecord:
		v = new(ImportRecord)
	case KindImportFile:
		v = new(ImportFile)
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", ErrSchema, kind)
	}
	if !isJSONObject(body) {
		return nil, fmt.Errorf("%w: %s body must be a JSON object", ErrSchema, kind)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return nil, fmt.Errorf("%w: %s body: %v", ErrSchema, kind, err)
	}
	return v, nil
}

// DecodeTombstone decodes and checks one CAB1 type-3 record: v (above ProtocolV1 wraps
// ErrUnsupportedProtocol), outbox_id of 32 lowercase hex, seq >= 1, ValidKey and a reason of
// rejected_local, rejected_too_large or missing_ref; anything else wraps ErrSchema. It is the
// tombstone counterpart of DecodeEnvelope.
func DecodeTombstone(raw []byte) (Tombstone, error) {
	var head struct {
		V json.Number `json:"v"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Tombstone{}, fmt.Errorf("%w: tombstone: %v", ErrSchema, err)
	}
	if err := checkVersion(head.V); err != nil {
		return Tombstone{}, err
	}
	var t Tombstone
	if err := json.Unmarshal(raw, &t); err != nil {
		return Tombstone{}, fmt.Errorf("%w: tombstone: %v", ErrSchema, err)
	}
	switch {
	case !validOutboxID(t.OutboxID):
		return Tombstone{}, fmt.Errorf("%w: tombstone outbox_id must be 32 lowercase hex", ErrSchema)
	case t.Seq < 1:
		return Tombstone{}, fmt.Errorf("%w: tombstone seq must be >= 1", ErrSchema)
	case !ValidKey(t.Key):
		return Tombstone{}, fmt.Errorf("%w: tombstone key invalid", ErrSchema)
	case t.Reason != TombRejectedLocal && t.Reason != TombRejectedTooLarge && t.Reason != TombMissingRef:
		return Tombstone{}, fmt.Errorf("%w: tombstone reason %q", ErrSchema, t.Reason)
	}
	return t, nil
}

// checkVersion applies the protocol version rule to a v field: above ProtocolV1 is ErrUnsupportedProtocol;
// missing, non-integer or below 1 is ErrSchema.
func checkVersion(v json.Number) error {
	if v == "" {
		return fmt.Errorf("%w: missing v", ErrSchema)
	}
	n, err := v.Int64()
	if err != nil {
		return fmt.Errorf("%w: v must be an integer", ErrSchema)
	}
	if n > ProtocolV1 {
		return fmt.Errorf("%w: v=%d", ErrUnsupportedProtocol, n)
	}
	if n < 1 {
		return fmt.Errorf("%w: v=%d", ErrSchema, n)
	}
	return nil
}

func validOutboxID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isLowerHex(s[i]) {
			return false
		}
	}
	return true
}

// isJSONObject reports whether raw (already valid JSON) is an object.
func isJSONObject(raw []byte) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")
	return len(t) > 0 && t[0] == '{'
}
