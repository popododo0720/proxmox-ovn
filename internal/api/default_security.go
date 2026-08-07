package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

func (s *Server) ensureDefaultSecurityGroup(writer http.ResponseWriter, ctx context.Context) (*model.SecurityGroup, bool) {
	group, err := s.defaultSecurity.Ensure(ctx)
	if err != nil {
		s.writeDefaultSecurityError(writer, err)
		return nil, false
	}
	return group, true
}

func (s *Server) writeDefaultSecurityError(writer http.ResponseWriter, err error) {
	if errors.Is(err, controlstore.ErrNotFound) || errors.Is(err, controlstore.ErrAlreadyExists) ||
		errors.Is(err, controlstore.ErrConflict) || errors.Is(err, controlstore.ErrPrecondition) ||
		errors.Is(err, controlstore.ErrIdempotencyConflict) {
		s.storeError(writer, err)
		return
	}
	s.logger.Error("default security policy is unavailable", "error", err)
	writeError(writer, http.StatusServiceUnavailable, "default_security_policy_unavailable", "the cluster default security policy is not ready", nil)
}

func (s *Server) preparePortSecurityGroups(writer http.ResponseWriter, ctx context.Context, port *model.Port, _ *model.Port) bool {
	if len(port.SecurityGroupIDs) != 0 {
		return true
	}
	group, ok := s.ensureDefaultSecurityGroup(writer, ctx)
	if !ok {
		return false
	}
	port.SecurityGroupIDs = []string{group.ID}
	return true
}

// EnsureDefaultSecurityPolicies repairs the one cluster-global default policy.
func (s *Server) EnsureDefaultSecurityPolicies(ctx context.Context) error {
	return s.defaultSecurity.EnsureAll(ctx)
}
