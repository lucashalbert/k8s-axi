package kube

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

// apiError turns a Kubernetes API error into a short structured error with a
// hint for the usual causes. verb and resource name what was attempted so a
// permission error can say what to grant.
func apiError(op, target string, err error, verb, resource string) error {
	hint := "check that the API server is reachable and the credentials are valid"
	switch {
	case apierrors.IsNotFound(err):
		hint = "check the name and namespace (-n), or list what exists with 'k8s-axi get " + resource + "'"
	case apierrors.IsConflict(err):
		hint = "the object changed since it was read; re-run the command to plan against its current state"
	case dryRunUnsupported(err):
		hint = "the API server does not support dry-run for " + resource + "; use 'k8s-axi plan' for a preview from the live state"
	case apierrors.IsForbidden(err):
		hint = "RBAC denied '" + verb + "' on " + resource + "; ask for that permission or use another --context"
	case apierrors.IsUnauthorized(err):
		hint = "the API server rejected the credentials; refresh the kubeconfig credentials"
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		hint = "the request timed out; raise --timeout or narrow the query"
	case apierrors.IsTooManyRequests(err):
		hint = "the API server is throttling requests; retry shortly"
	}
	return axierr.Runtime(op, target, redactedError(err), hint)
}
