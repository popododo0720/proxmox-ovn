package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/popododo0720/proxmox-ovn/internal/controlstore"
	"github.com/popododo0720/proxmox-ovn/internal/defaultsecurity"
	"github.com/popododo0720/proxmox-ovn/internal/model"
)

const (
	defaultSecurityGroupBackfillPlanPath  = "/api/v1/admin/default-security-group-backfill/plan"
	defaultSecurityGroupBackfillApplyPath = "/api/v1/admin/default-security-group-backfill/apply"
	defaultSecurityGroupBackfillWarning   = "Applying this backfill changes traffic policy immediately for attached ports; review attached VM NICs before setting dry_run to false."
)

type defaultSecurityGroupBackfillPlanData struct {
	Cluster            string                                `json:"cluster"`
	GeneratedAt        string                                `json:"generated_at"`
	Warning            string                                `json:"warning"`
	TotalLegacyPorts   int                                   `json:"total_legacy_ports"`
	TotalAttachedPorts int                                   `json:"total_attached_ports"`
	CanApply           bool                                  `json:"can_apply"`
	Projects           []defaultSecurityGroupBackfillProject `json:"projects"`
}

type defaultSecurityGroupBackfillProject struct {
	ProjectID                string                             `json:"project_id"`
	ProjectName              string                             `json:"project_name"`
	DefaultSecurityGroupID   string                             `json:"default_security_group_id"`
	DefaultSecurityGroupName string                             `json:"default_security_group_name"`
	DefaultReady             bool                               `json:"default_ready"`
	MissingResourceIDs       []string                           `json:"missing_resource_ids,omitempty"`
	BlockedReason            defaultsecurity.BlockedReason      `json:"blocked_reason,omitempty"`
	Detail                   string                             `json:"detail,omitempty"`
	LegacyPorts              []defaultSecurityGroupBackfillPort `json:"legacy_ports"`
	inspection               defaultsecurity.Inspection
}

type defaultSecurityGroupBackfillPort struct {
	PortID   string `json:"port_id"`
	PortName string `json:"port_name"`
	Revision int64  `json:"revision"`
	Attached bool   `json:"attached"`
	NodeID   string `json:"node_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	VMID     int    `json:"vmid,omitempty"`
	NIC      string `json:"nic,omitempty"`
	port     *model.Port
}

type defaultSecurityGroupBackfillApplyRequest struct {
	DryRun  *bool  `json:"dry_run,omitempty"`
	Confirm string `json:"confirm,omitempty"`
}

type defaultSecurityGroupBackfillApplyData struct {
	Cluster  string                                   `json:"cluster"`
	DryRun   bool                                     `json:"dry_run"`
	Warning  string                                   `json:"warning"`
	Planned  int                                      `json:"planned"`
	Migrated int                                      `json:"migrated"`
	Skipped  int                                      `json:"skipped"`
	Failed   int                                      `json:"failed"`
	Results  []defaultSecurityGroupBackfillPortResult `json:"results"`
}

type defaultSecurityGroupBackfillPortResult struct {
	ProjectID      string `json:"project_id"`
	ProjectName    string `json:"project_name"`
	PortID         string `json:"port_id"`
	PortName       string `json:"port_name"`
	Attached       bool   `json:"attached"`
	Status         string `json:"status"`
	RevisionBefore int64  `json:"revision_before"`
	RevisionAfter  int64  `json:"revision_after,omitempty"`
	Detail         string `json:"detail,omitempty"`
	Error          string `json:"error,omitempty"`
}

func (s *Server) defaultSecurityGroupBackfillPlan(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !s.requireDefaultSecurityGroupBackfillPrivileges(writer, request, "SDN.Audit") {
		return
	}
	plan, err := s.buildDefaultSecurityGroupBackfillPlan(request.Context())
	if err != nil {
		s.storeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"data": plan})
}

func (s *Server) defaultSecurityGroupBackfillApply(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !s.requireDefaultSecurityGroupBackfillPrivileges(writer, request, "SDN.Allocate", "Sys.Modify") {
		return
	}
	var input defaultSecurityGroupBackfillApplyRequest
	if !decodeActionBody(writer, request, &input) {
		return
	}
	dryRun := true
	if input.DryRun != nil {
		dryRun = *input.DryRun
	}
	if !dryRun && (s.clusterName == "" || input.Confirm != s.clusterName) {
		writeError(writer, http.StatusBadRequest, "confirmation_required", "apply requires confirm to exactly match the configured PVN cluster name", map[string]any{"cluster": s.clusterName})
		return
	}
	plan, err := s.buildDefaultSecurityGroupBackfillPlan(request.Context())
	if err != nil {
		s.storeError(writer, err)
		return
	}
	report := s.applyDefaultSecurityGroupBackfill(request.Context(), plan, dryRun)
	writeJSON(writer, http.StatusOK, map[string]any{"data": report})
}

func (s *Server) requireDefaultSecurityGroupBackfillPrivileges(writer http.ResponseWriter, request *http.Request, privileges ...string) bool {
	session, ok := request.Context().Value(sessionContextKey{}).(Session)
	if !ok || session.User == "" {
		writeError(writer, http.StatusUnauthorized, "unauthenticated", "a valid Proxmox session is required", nil)
		return false
	}
	for _, privilege := range privileges {
		if !hasPrivilege(session, globalPath, privilege) {
			writeError(writer, http.StatusForbidden, "forbidden", fmt.Sprintf("global %s is required", privilege), nil)
			return false
		}
	}
	return true
}

func (s *Server) buildDefaultSecurityGroupBackfillPlan(ctx context.Context) (*defaultSecurityGroupBackfillPlanData, error) {
	snapshot, err := s.store.Snapshot(ctx, []model.Kind{model.KindProject, model.KindPort, model.KindNode}, controlstore.ListOptions{})
	if err != nil {
		return nil, err
	}

	nodeNames := make(map[string]string, len(snapshot[model.KindNode]))
	for _, resource := range snapshot[model.KindNode] {
		node := resource.(*model.Node)
		nodeNames[node.ID] = node.Name
	}
	portsByProject := make(map[string][]*model.Port)
	for _, resource := range snapshot[model.KindPort] {
		port := resource.(*model.Port)
		if len(port.SecurityGroupIDs) == 0 {
			portsByProject[port.ProjectID] = append(portsByProject[port.ProjectID], port)
		}
	}

	projects := make([]*model.Project, 0, len(snapshot[model.KindProject]))
	for _, resource := range snapshot[model.KindProject] {
		projects = append(projects, resource.(*model.Project))
	}
	sort.Slice(projects, func(left, right int) bool {
		if projects[left].Name != projects[right].Name {
			return projects[left].Name < projects[right].Name
		}
		return projects[left].ID < projects[right].ID
	})

	policyManager := defaultsecurity.New(s.store, s.reconciler)
	plan := &defaultSecurityGroupBackfillPlanData{
		Cluster:     s.clusterName,
		GeneratedAt: s.clusterGate.now().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		Warning:     defaultSecurityGroupBackfillWarning,
		Projects:    make([]defaultSecurityGroupBackfillProject, 0, len(projects)),
	}
	for _, project := range projects {
		inspection, inspectErr := policyManager.Inspect(ctx, project.ID)
		if inspectErr != nil {
			return nil, inspectErr
		}
		projectPlan := defaultSecurityGroupBackfillProject{
			ProjectID:                project.ID,
			ProjectName:              project.Name,
			DefaultSecurityGroupID:   inspection.GroupID,
			DefaultSecurityGroupName: defaultsecurity.DefaultSecurityGroupName,
			DefaultReady:             inspection.Ready,
			MissingResourceIDs:       append([]string(nil), inspection.MissingResourceIDs...),
			BlockedReason:            inspection.BlockedReason,
			Detail:                   inspection.Detail,
			LegacyPorts:              []defaultSecurityGroupBackfillPort{},
			inspection:               inspection,
		}
		if !inspection.Ready && inspection.BlockedReason == defaultsecurity.BlockedNone && len(inspection.MissingResourceIDs) == 0 && projectPlan.Detail == "" {
			projectPlan.Detail = "default security policy requires reconciliation"
		}
		legacyPorts := portsByProject[project.ID]
		sort.Slice(legacyPorts, func(left, right int) bool {
			if legacyPorts[left].Name != legacyPorts[right].Name {
				return legacyPorts[left].Name < legacyPorts[right].Name
			}
			return legacyPorts[left].ID < legacyPorts[right].ID
		})
		for _, port := range legacyPorts {
			attached := portAttached(port)
			projectPlan.LegacyPorts = append(projectPlan.LegacyPorts, defaultSecurityGroupBackfillPort{
				PortID: port.ID, PortName: port.Name, Revision: port.Revision, Attached: attached,
				NodeID: port.NodeID, NodeName: nodeNames[port.NodeID], VMID: port.VMID, NIC: port.NIC, port: port,
			})
			plan.TotalLegacyPorts++
			if attached {
				plan.TotalAttachedPorts++
			}
		}
		if len(projectPlan.LegacyPorts) > 0 && inspection.BlockedReason == defaultsecurity.BlockedNone {
			plan.CanApply = true
		}
		plan.Projects = append(plan.Projects, projectPlan)
	}
	return plan, nil
}

func (s *Server) applyDefaultSecurityGroupBackfill(ctx context.Context, plan *defaultSecurityGroupBackfillPlanData, dryRun bool) *defaultSecurityGroupBackfillApplyData {
	report := &defaultSecurityGroupBackfillApplyData{
		Cluster: plan.Cluster, DryRun: dryRun, Warning: plan.Warning,
		Results: make([]defaultSecurityGroupBackfillPortResult, 0, plan.TotalLegacyPorts),
	}
	policyManager := defaultsecurity.New(s.store, s.reconciler)
	for _, project := range plan.Projects {
		if len(project.LegacyPorts) == 0 {
			continue
		}
		report.Planned += len(project.LegacyPorts)
		if project.inspection.BlockedReason != defaultsecurity.BlockedNone {
			for _, port := range project.LegacyPorts {
				report.Results = append(report.Results, failedDefaultSecurityGroupBackfillResult(project, port, project.Detail))
				report.Failed++
			}
			continue
		}
		if dryRun {
			for _, port := range project.LegacyPorts {
				report.Results = append(report.Results, defaultSecurityGroupBackfillPortResult{
					ProjectID: project.ProjectID, ProjectName: project.ProjectName,
					PortID: port.PortID, PortName: port.PortName, Attached: port.Attached,
					Status: "planned", RevisionBefore: port.Revision,
				})
			}
			continue
		}

		group, ensureErr := policyManager.Ensure(ctx, project.ProjectID)
		if ensureErr != nil {
			for _, port := range project.LegacyPorts {
				report.Results = append(report.Results, failedDefaultSecurityGroupBackfillResult(project, port, ensureErr.Error()))
				report.Failed++
			}
			continue
		}
		ready, inspectErr := policyManager.Inspect(ctx, project.ProjectID)
		if inspectErr != nil || !ready.Ready {
			detail := "default security policy was not fully realized"
			if inspectErr != nil {
				detail = inspectErr.Error()
			} else if ready.Detail != "" {
				detail += ": " + ready.Detail
			}
			for _, port := range project.LegacyPorts {
				report.Results = append(report.Results, failedDefaultSecurityGroupBackfillResult(project, port, detail))
				report.Failed++
			}
			continue
		}
		for _, port := range project.LegacyPorts {
			result := s.migrateLegacyPortToDefaultSecurityGroup(ctx, project, port, group)
			report.Results = append(report.Results, result)
			switch result.Status {
			case "migrated":
				report.Migrated++
			case "skipped":
				report.Skipped++
			default:
				report.Failed++
			}
		}
	}
	return report
}

func (s *Server) migrateLegacyPortToDefaultSecurityGroup(ctx context.Context, project defaultSecurityGroupBackfillProject, candidate defaultSecurityGroupBackfillPort, group *model.SecurityGroup) defaultSecurityGroupBackfillPortResult {
	result := defaultSecurityGroupBackfillPortResult{
		ProjectID: project.ProjectID, ProjectName: project.ProjectName,
		PortID: candidate.PortID, PortName: candidate.PortName, Attached: candidate.Attached,
		Status: "failed", RevisionBefore: candidate.Revision,
	}
	desiredResource, err := model.Clone(candidate.port)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	desired := desiredResource.(*model.Port)
	desired.SecurityGroupIDs = []string{group.ID}
	updated, _, err := s.store.Update(ctx, desired, candidate.Revision, "")
	if err != nil {
		return s.classifyDefaultSecurityGroupBackfillUpdateFailure(ctx, result, group.ID, err)
	}
	result.Status = "migrated"
	result.RevisionAfter = updated.GetMetadata().Revision
	if s.reconciler != nil {
		if reconcileErr := s.reconciler.Reconcile(ctx, model.KindPort, candidate.PortID); reconcileErr != nil {
			result.Detail = "default security group assigned; OVN reconciliation failed and will be retried: " + reconcileErr.Error()
		}
	}
	return result
}

func (s *Server) classifyDefaultSecurityGroupBackfillUpdateFailure(ctx context.Context, result defaultSecurityGroupBackfillPortResult, groupID string, updateErr error) defaultSecurityGroupBackfillPortResult {
	if !errors.Is(updateErr, controlstore.ErrPrecondition) {
		result.Error = updateErr.Error()
		return result
	}
	latestResource, getErr := s.store.Get(ctx, model.KindPort, result.PortID)
	if getErr != nil {
		result.Error = fmt.Sprintf("revision changed concurrently and the current port could not be loaded: %v", getErr)
		return result
	}
	latest := latestResource.(*model.Port)
	result.RevisionAfter = latest.Revision
	if len(latest.SecurityGroupIDs) == 1 && latest.SecurityGroupIDs[0] == groupID {
		result.Status = "skipped"
		result.Detail = "another backfill worker already assigned the default security group"
		return result
	}
	if len(latest.SecurityGroupIDs) > 0 {
		result.Status = "skipped"
		result.Detail = "security groups changed concurrently; the current assignment was left unchanged"
		return result
	}
	result.Error = updateErr.Error()
	return result
}

func failedDefaultSecurityGroupBackfillResult(project defaultSecurityGroupBackfillProject, port defaultSecurityGroupBackfillPort, detail string) defaultSecurityGroupBackfillPortResult {
	if detail == "" {
		detail = "the project's default security policy is blocked"
	}
	return defaultSecurityGroupBackfillPortResult{
		ProjectID: project.ProjectID, ProjectName: project.ProjectName,
		PortID: port.PortID, PortName: port.PortName, Attached: port.Attached,
		Status: "failed", RevisionBefore: port.Revision, Error: detail,
	}
}
