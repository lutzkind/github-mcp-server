package github

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	gogithub "github.com/google/go-github/v89/github"
)

func retryGitHubCall[T any](
	ctx context.Context,
	deps ToolDependencies,
	operation string,
	fn func(context.Context) (T, *gogithub.Response, error),
) (T, *gogithub.Response, error) {
	var zero T
	delays := []time.Duration{0, 250 * time.Millisecond, time.Second}

	var lastValue T
	var lastResp *gogithub.Response
	var lastErr error

	for attempt, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return zero, lastResp, ctx.Err()
			case <-time.After(delay):
			}
		}

		value, resp, err := fn(ctx)
		lastValue = value
		lastResp = resp
		lastErr = err

		if !isRetryableGitHubFailure(resp, err) || attempt == len(delays)-1 {
			return value, resp, err
		}

		statusCode := 0
		if resp != nil && resp.Response != nil {
			statusCode = resp.StatusCode
		}
		deps.Logger(ctx).Warn(
			"retrying GitHub API call after transient failure",
			"operation", operation,
			"attempt", attempt+1,
			"status_code", statusCode,
			"error", errString(err),
		)
	}

	return lastValue, lastResp, lastErr
}

func retryGitHubHTTPCall(
	ctx context.Context,
	deps ToolDependencies,
	operation string,
	fn func(context.Context) (*http.Response, error),
) (*http.Response, error) {
	delays := []time.Duration{0, 250 * time.Millisecond, time.Second}

	var lastResp *http.Response
	var lastErr error

	for attempt, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := fn(ctx)
		lastResp = resp
		lastErr = err

		if !isRetryableHTTPFailure(resp, err) || attempt == len(delays)-1 {
			return resp, err
		}

		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		deps.Logger(ctx).Warn(
			"retrying GitHub raw HTTP call after transient failure",
			"operation", operation,
			"attempt", attempt+1,
			"status_code", statusCode,
			"error", errString(err),
		)
	}

	return lastResp, lastErr
}

func isRetryableGitHubFailure(resp *gogithub.Response, err error) bool {
	if err != nil && isRetryableNetworkError(err) {
		return true
	}
	if resp == nil || resp.Response == nil {
		return false
	}
	return isRetryableStatus(resp.StatusCode)
}

func isRetryableHTTPFailure(resp *http.Response, err error) bool {
	if err != nil && isRetryableNetworkError(err) {
		return true
	}
	if resp == nil {
		return false
	}
	return isRetryableStatus(resp.StatusCode)
}

func isRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}

	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timeout") ||
		strings.Contains(message, "connection reset") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "stream error") ||
		strings.Contains(message, "http2")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
