package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	pluginimpl "github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/plugin"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// A method the plugin does not handle is answered as one, unknown_method
// rather than plugin_error, with a fixed message: the method name is CPA's,
// and the code already says what went wrong.
func TestAnUnknownMethodIsReportedAsOne(t *testing.T) {
	_, err := handleGuarded(pluginimpl.New(nil), "plugin.not-a-method", nil)
	if err == nil {
		t.Fatal("an unknown method was handled")
	}
	if code := errorCode(err); code != "unknown_method" {
		t.Fatalf("code = %q, want unknown_method", code)
	}
	if message := sanitizeError(err); message != "unknown method" {
		t.Fatalf("message = %q, want the fixed %q", message, "unknown method")
	}
}

// The operator sees why the panel's value was refused, not the generic
// failure every unlisted message becomes.
func TestAWarnBelowThatIsNotAnAmountIsNamedToTheOperator(t *testing.T) {
	raw, _ := json.Marshal(protocol.LifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("openrouter-warn-below: lots\n")})
	_, err := handleGuarded(pluginimpl.New(nil), protocol.MethodPluginRegister, raw)
	if err == nil {
		t.Fatal("openrouter-warn-below: lots was accepted")
	}
	if code, message := errorCode(err), sanitizeError(err); code != "plugin_error" || message != "openrouter-warn-below must be a dollar amount of 0 or more" {
		t.Fatalf("answered %s %q", code, message)
	}
}

// configurePath is every function a register or reconfigure returns an error
// from, keyed by its file from the module root: Handle and configure, and what
// configure calls that can fail.
var configurePath = map[string][]string{
	"internal/plugin/plugin.go": {"Handle", "configure", "resolveToken"},
	"internal/store/store.go":   {"Open"},
	"internal/watch/watch.go":   {"Start"},
}

// Every error on that path is a fixed message, and each one reaches the
// operator as written: sanitizeError turns any message it does not list into
// "quota glance request failed", which names no cause at all. A message that
// is built at run time could carry a path or a value, so none may be, apart
// from the unknown method, which is answered with a fixed one.
func TestEveryFixedConfigureErrorReachesTheOperator(t *testing.T) {
	files := token.NewFileSet()
	for file, names := range configurePath {
		parsed, err := parser.ParseFile(files, filepath.FromSlash(file), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			var body *ast.BlockStmt
			for _, decl := range parsed.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
					body = fn.Body
				}
			}
			if body == nil {
				t.Fatalf("%s has no func %s; update configurePath", file, name)
			}
			messages := 0
			ast.Inspect(body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok || !(pkg.Name == "errors" && selector.Sel.Name == "New" || pkg.Name == "fmt" && selector.Sel.Name == "Errorf") {
					return true
				}
				where := files.Position(call.Pos())
				literal, ok := call.Args[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Errorf("%s: an error message that is not a literal", where)
					return true
				}
				message, _ := strconv.Unquote(literal.Value)
				if selector.Sel.Name == "Errorf" {
					if !strings.HasPrefix(message, unknownMethodPrefix) {
						t.Errorf("%s: %q is built at run time", where, message)
					}
					return true
				}
				messages++
				if got := sanitizeError(errors.New(message)); got != message {
					t.Errorf("%s: %q reaches the operator as %q", where, message, got)
				}
				return true
			})
			if messages == 0 {
				t.Errorf("%s: func %s returns no fixed message; update configurePath", file, name)
			}
		}
	}
}

// An older CPA without host.routing.reset_cooldown is told apart from a call
// that failed, and neither error carries the host's message, which elsewhere
// can hold upstream text.
func TestCallbackFailureNamesAnUnsupportedCallbackAndNothingElse(t *testing.T) {
	missing := callbackFailure(protocol.MethodHostRoutingResetCooldown, &protocol.EnvelopeError{
		Code: "host_call_failed", Message: "unsupported host callback host.routing.reset_cooldown",
	})
	if !errors.Is(missing, protocol.ErrUnsupportedCallback) {
		t.Fatalf("missing callback = %v, want ErrUnsupportedCallback", missing)
	}
	failed := callbackFailure(protocol.MethodHostRoutingResetCooldown, &protocol.EnvelopeError{
		Code: "host_call_failed", Message: "auth not found for auth_index secret-ish-detail",
	})
	if errors.Is(failed, protocol.ErrUnsupportedCallback) || strings.Contains(failed.Error(), "secret-ish-detail") {
		t.Fatalf("failed call = %v, want a plain failure without the host's message", failed)
	}
	if bare := callbackFailure(protocol.MethodHostRoutingResetCooldown, nil); bare == nil || errors.Is(bare, protocol.ErrUnsupportedCallback) {
		t.Fatalf("bare failure = %v", bare)
	}
}

// The callback's name and wire shape, as CPA declares them in
// sdk/pluginabi/types.go (MethodHostRoutingResetCooldown) and
// sdk/pluginapi/types.go (HostRoutingResetCooldownRequest and Response). The
// bridge itself only runs inside CPA, so this is where a drift in the mirror
// would show.
func TestTheCooldownCallbackMatchesCPAsContract(t *testing.T) {
	if protocol.MethodHostRoutingResetCooldown != "host.routing.reset_cooldown" {
		t.Fatalf("method = %q", protocol.MethodHostRoutingResetCooldown)
	}
	raw, err := json.Marshal(protocol.HostRoutingResetCooldownRequest{AuthIndex: "a1b2c3"})
	if err != nil || string(raw) != `{"auth_index":"a1b2c3"}` {
		t.Fatalf("request = %s, %v", raw, err)
	}
	var response protocol.HostRoutingResetCooldownResponse
	if err := json.Unmarshal([]byte(`{"auth_index":"a1b2c3","models":["claude-haiku-5-5"]}`), &response); err != nil ||
		response.AuthIndex != "a1b2c3" || len(response.Models) != 1 {
		t.Fatalf("response = %+v, %v", response, err)
	}
}

// The goroutine that cancels an abandoned host operation is one nothing above
// recovers. A cancel that panics must end there, not abort CPA; and the cancel
// runs only for a request whose context ended before it finished.
func TestACancelThatPanicsStaysOnItsGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var called atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		cancelOnDone(ctx, make(chan struct{}), func() {
			called.Store(true)
			panic("operation op-1 holds a token")
		})
	}()
	cancel()
	<-done
	if !called.Load() {
		t.Fatal("an ended context did not cancel the operation")
	}

	finished := make(chan struct{})
	close(finished)
	cancelOnDone(context.Background(), finished, func() { t.Fatal("a finished request was cancelled") })
}
