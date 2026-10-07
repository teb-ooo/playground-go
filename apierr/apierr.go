// Package apierr keeps cause text out of 5xx responses. Huma's default error constructor puts a plain error's
// text into the problem+json body (errors[].message), so a handler that returns a wrapped pgx or pgconn error
// would show SQL to the client. Install replaces huma.NewError and huma.NewErrorWithContext so that any error
// with status >= 500 becomes a fixed problem+json body (title is the status text, detail is "internal error",
// no errors list) and the original message and causes are logged with slog at error level instead, with the
// request id when the log middleware ran. Errors below 500, such as huma.Error404NotFound("no such item"), keep
// their messages, but a validation error never echoes the offending value: Huma's errors[].value would repeat what the
// client sent (a whole request field, up to the body size), which for a user-class app is user content in the
// response and wherever a client logs it (rule DAT-cld). The message and the location stay.
//
// Install runs from the init of the root playground package, so every app that imports it (all do, for Config)
// has it after upgrading. Calling Install again is a no-op.
package apierr

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	playgroundlog "github.com/teb-ooo/playground-go/log"
)

// Detail is the body detail of every 5xx response.
const Detail = "internal error"

var once sync.Once

// Install makes 5xx responses generic. It is idempotent and safe to call from init.
func Install() {
	once.Do(func() {
		prev := huma.NewError
		huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
			if status >= http.StatusInternalServerError {
				logCause(context.Background(), status, msg, errs)
				return prev(status, Detail)
			}
			return prev(status, msg, withoutValues(errs)...)
		}
		huma.NewErrorWithContext = func(ctx huma.Context, status int, msg string, errs ...error) huma.StatusError {
			if status >= http.StatusInternalServerError {
				logCause(ctx.Context(), status, msg, errs)
				return prev(status, Detail)
			}
			return huma.NewError(status, msg, errs...)
		}
	})
}

// withoutValues returns errs with the offending value dropped from every Huma validation detail (message and location kept).
func withoutValues(errs []error) []error {
	var out []error
	for i, e := range errs {
		if d, ok := e.(*huma.ErrorDetail); ok && d != nil && d.Value != nil {
			if out == nil {
				out = append([]error(nil), errs...)
			}
			out[i] = &huma.ErrorDetail{Message: d.Message, Location: d.Location}
		}
	}
	if out == nil {
		return errs
	}
	return out
}

func logCause(ctx context.Context, status int, msg string, errs []error) {
	attrs := []any{slog.Int("status", status), slog.String("detail", msg)}
	for i, e := range errs {
		if e != nil {
			attrs = append(attrs, slog.String(causeKey(i), e.Error()))
		}
	}
	playgroundlog.Logger(ctx).ErrorContext(ctx, "internal error", attrs...)
}

func causeKey(i int) string {
	if i == 0 {
		return "error"
	}
	return "error_" + string(rune('0'+i%10))
}
