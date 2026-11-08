package capture

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/outbox"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// ErrRemaskBase: re-masking changed item ids of a prefix-delta begin, whose ListSHA covers base
// items this envelope does not hold; the dead letter is left in place, because recomputing ListSHA
// would need the base chain.
var ErrRemaskBase = errors.New("capture: remask changes a prefix-delta begin")

// ErrRemaskBudget: the content pool cannot cover remasking a piece (4 × its size, or its JSON
// values over the node cap); the dead letter is left in place and a later requeue may succeed.
var ErrRemaskBudget = errors.New("capture: remask over the content-pool budget")

// PieceLoader reads a stored piece.
type PieceLoader func(wire.Hash) (wire.PieceKind, string, []byte, error)

// Requeued builds the replacement of a dead letter for the requeue route:
// same kind, key and created_at. With remask, every text and json piece it references and the
// body are masked again with the current masker (idempotent on placeholders); changed pieces get
// new hashes, every reference to them is rewritten, and N/ListSHA are recomputed. Attachments are
// kept as stored.
//
// Remasking runs under the capture budgets: each loaded piece reserves 4 × its size from pool
// before it is masked (released at once when masking leaves it unchanged, else kept for the
// rewritten piece), every piece and the body are masked under ctx and MaskTimeout, and a json
// piece's values are capped like a split's (reservation plus free pool bytes, 250 per value).
// held is the pool bytes still reserved for the returned pieces; the caller releases them once
// the outbox wrote the envelope. On error nothing stays reserved. ctx is also checked between
// pieces, so a stop never waits for a whole envelope.
func Requeued(ctx context.Context, raw []byte, remask bool, load PieceLoader, h *MaskerHolder, pool *hooks.Pool) (out outbox.NewEnvelope, held int64, err error) {
	env, err := wire.DecodeEnvelope(raw)
	if err != nil {
		return outbox.NewEnvelope{}, 0, err
	}
	out = outbox.NewEnvelope{Kind: env.Kind, Key: env.Key, CreatedAt: env.CreatedAt, ExactRev: env.ExactRev, Refs: env.Refs, Body: env.Body}
	if !remask {
		return out, 0, nil
	}
	defer func() {
		if err != nil {
			pool.Release(held)
			held = 0
		}
	}()
	ms := h.Get()
	repl := map[wire.Hash]wire.Hash{}
	var pieces []canon.Piece
	for _, ref := range env.Refs {
		if err = ctx.Err(); err != nil {
			return outbox.NewEnvelope{}, held, err
		}
		kind, ct, b, lerr := load(ref)
		if lerr != nil {
			return outbox.NewEnvelope{}, held, lerr
		}
		if kind != wire.PieceText && kind != wire.PieceJSON {
			continue // attachments are kept as stored
		}
		need := int64(len(b)) * hooks.BodyReserveFactor
		if !pool.TryReserve(need) {
			return outbox.NewEnvelope{}, held, ErrRemaskBudget
		}
		nb, merr := remaskPiece(ctx, kind, b, ms, need+pool.Free())
		if merr != nil {
			pool.Release(need)
			return outbox.NewEnvelope{}, held, merr
		}
		if bytes.Equal(nb, b) {
			pool.Release(need)
			continue
		}
		keep := min(int64(len(nb)), need)
		pool.Release(need - keep)
		held += keep
		p := canon.NewPiece(kind, ct, nb)
		repl[ref] = p.Hash
		pieces = append(pieces, p)
	}
	body := env.Body
	for o, n := range repl {
		body = bytes.ReplaceAll(body, []byte(`"`+o.String()+`"`), []byte(`"`+n.String()+`"`))
	}
	bctx, cancel := context.WithTimeout(ctx, MaskTimeout)
	body, _, _, err = canon.CanonicalizeMaskedContext(bctx, body, ms.Masker, 0)
	cancel()
	if err != nil {
		return outbox.NewEnvelope{}, held, err
	}
	if env.Kind == wire.KindExchangeBegin || env.Kind == wire.KindExchangeResponse {
		if body, err = relist(env.Kind, env.Body, body, ms); err != nil {
			return outbox.NewEnvelope{}, held, err
		}
	}
	refs := make([]wire.Hash, len(env.Refs))
	for i, r := range env.Refs {
		if n, ok := repl[r]; ok {
			refs[i] = n
		} else {
			refs[i] = r
		}
	}
	out.Body, out.Refs, out.Pieces, out.ExactRev = body, refs, pieces, ms.Rev
	return out, held, nil
}

// remaskPiece masks one text or json piece again under ctx and MaskTimeout, so a stop or the
// deadline ends even one long piece. f is the bytes the remask may claim (its reservation plus the
// pool's free bytes), which caps a json piece's values as a split's are (250 bytes per value).
func remaskPiece(ctx context.Context, kind wire.PieceKind, b []byte, ms *MaskerState, f int64) ([]byte, error) {
	mctx, cancel := context.WithTimeout(ctx, MaskTimeout)
	defer cancel()
	if kind == wire.PieceText {
		s, _, err := ms.Masker.StringContext(mctx, string(b))
		if err != nil {
			return nil, err
		}
		return []byte(s), nil
	}
	nb, _, _, err := canon.CanonicalizeMaskedContext(mctx, b, ms.Masker, int(max(f/250, 1)))
	if errors.Is(err, canon.ErrBodyBudget) {
		return nil, ErrRemaskBudget
	}
	return nb, err
}

// relist recomputes N and ListSHA of a begin or response body whose items changed.
func relist(kind wire.Kind, oldBody, newBody []byte, ms *MaskerState) ([]byte, error) {
	oldV, err := wire.DecodeBody(kind, oldBody)
	if err != nil {
		return nil, err
	}
	newV, err := wire.DecodeBody(kind, newBody)
	if err != nil {
		return nil, err
	}
	var ob, nb *wire.Body
	switch n := newV.(type) {
	case *wire.ExchangeBegin:
		ob, nb = &oldV.(*wire.ExchangeBegin).Body, &n.Body
	case *wire.ExchangeResponse:
		ob, nb = &oldV.(*wire.ExchangeResponse).Body, &n.Body
	default:
		return newBody, nil
	}
	oldIDs, newIDs := canon.ItemIDs(ob.Items), canon.ItemIDs(nb.Items)
	if slices.Equal(oldIDs, newIDs) {
		return newBody, nil
	}
	if nb.Base != nil {
		return nil, ErrRemaskBase
	}
	nb.N = uint32(len(newIDs))
	nb.ListSHA = canon.ListSHA(newIDs)
	return finalBody(newV, ms.Masker)
}
