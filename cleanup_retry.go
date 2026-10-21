package ghalet

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"syscall"

	"github.com/actions/scaleset"
)

// The pinned scaleset SDK exposes HTTP status only in its error text.
var cleanupHTTPStatus = regexp.MustCompile(`^(?:request \S+ \S+ failed\(status="|unexpected HTTP status )([0-9]{3})(?: |")`)

// With retries disabled, retryablehttp drops the response for 429 and returns
// only this exhaustion error. Other retryable statuses retain a wrapped cause.
var cleanupRateLimited = regexp.MustCompile(`^(?:GET|POST|DELETE) \S+ giving up after [0-9]+ attempt\(s\)$`)

func cleanupRetryable(err error) bool {
	for _, permanent := range []error{
		context.Canceled, os.ErrPermission, os.ErrInvalid,
		scaleset.UnauthorizedError, scaleset.BadRequestError, scaleset.NotFoundError,
		scaleset.ConflictError, scaleset.JobStillRunningError, scaleset.RunnerExistsError,
	} {
		if errors.Is(err, permanent) {
			return false
		}
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if match := cleanupHTTPStatus.FindStringSubmatch(cause.Error()); match != nil {
			status, _ := strconv.Atoi(match[1])
			return status == 408 || status == 429 || (status >= 500 && status != 501)
		}
		if cleanupRateLimited.MatchString(cause.Error()) {
			return true
		}
	}
	for _, transient := range []error{
		context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF,
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE,
		syscall.ENETDOWN, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ETIMEDOUT,
		syscall.EAGAIN, syscall.EINTR, syscall.EBUSY, syscall.ENOSPC, syscall.EDQUOT,
		syscall.EMFILE, syscall.ENFILE, syscall.EIO,
	} {
		if errors.Is(err, transient) {
			return true
		}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) && dnsError.IsTemporary {
		return true
	}
	return false
}
