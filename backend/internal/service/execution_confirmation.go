package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/constants"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/dto"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/model"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/repository"
	"gorm.io/gorm"
)

type ExecutionConfirmationService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.ExecutionConfirmation], error)
	Get(context.Context, uint) (model.ExecutionConfirmation, error)
	Create(context.Context, dto.CreateExecutionConfirmation, string, string) (model.ExecutionConfirmation, error)
	Update(context.Context, uint, dto.UpdateExecutionConfirmation, string, string) (model.ExecutionConfirmation, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string) (model.ExecutionConfirmation, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type executionConfirmationService struct {
	repository repository.ExecutionConfirmationRepository
	directives repository.OperationDirectiveRepository
	gates      repository.GateUnitRepository
	security   SecurityService
}

func NewExecutionConfirmationService(repo repository.ExecutionConfirmationRepository, directives repository.OperationDirectiveRepository, gates repository.GateUnitRepository, security SecurityService) ExecutionConfirmationService {
	return &executionConfirmationService{repository: repo, directives: directives, gates: gates, security: security}
}

func (s *executionConfirmationService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.ExecutionConfirmation], error) {
	page, err := s.repository.List(ctx, query)
	if err != nil {
		return repository.Page[model.ExecutionConfirmation]{}, err
	}
	if err := s.attachVerification(ctx, page.Items); err != nil {
		return repository.Page[model.ExecutionConfirmation]{}, err
	}
	return page, nil
}

func (s *executionConfirmationService) Get(ctx context.Context, id uint) (model.ExecutionConfirmation, error) {
	item, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ExecutionConfirmation{}, err
	}
	items := []model.ExecutionConfirmation{item}
	if err := s.attachVerification(ctx, items); err != nil {
		return model.ExecutionConfirmation{}, err
	}
	return items[0], nil
}

func (s *executionConfirmationService) Create(ctx context.Context, input dto.CreateExecutionConfirmation, actor, requestID string) (model.ExecutionConfirmation, error) {
	if err := validateExecutionConfirmationBusinessFields(input.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.ExecutionConfirmation{}, err
	}
	directive, err := s.requireExecutingDirective(ctx, input.RelatedCode)
	if err != nil {
		return model.ExecutionConfirmation{}, err
	}
	count, err := s.repository.CountByDirectiveCode(ctx, directive.Code)
	if err != nil {
		return model.ExecutionConfirmation{}, fmt.Errorf("check existing confirmation: %w", err)
	}
	if count > 0 {
		return model.ExecutionConfirmation{}, fmt.Errorf("%w: directive already has an execution confirmation", ErrInvalidInput)
	}
	item := model.ExecutionConfirmation{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.ExecutionConfirmationInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: directive.Facility, Owner: strings.TrimSpace(input.Owner),
		Category: directive.Category, RiskLevel: directive.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode:       directive.Code,
		MeasuredGateState: strings.TrimSpace(input.MeasuredGateState),
		ObservedAt:        input.ObservedAt.UTC(),
	}
	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Create(txCtx, &item); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "create", "ExecutionConfirmation", item.ID, "", item.Status, "created 执行确认")
	}); err != nil {
		return model.ExecutionConfirmation{}, fmt.Errorf("create 执行确认: %w", err)
	}
	return s.Get(ctx, item.ID)
}

func (s *executionConfirmationService) Update(ctx context.Context, id uint, input dto.UpdateExecutionConfirmation, actor, requestID string) (model.ExecutionConfirmation, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ExecutionConfirmation{}, err
	}
	if err := validateExecutionConfirmationBusinessFields(current.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.ExecutionConfirmation{}, err
	}
	if current.Status != model.ExecutionConfirmationInitialStatus {
		return model.ExecutionConfirmation{}, ErrImmutableState
	}
	if !strings.EqualFold(strings.TrimSpace(input.RelatedCode), current.RelatedCode) {
		return model.ExecutionConfirmation{}, fmt.Errorf("%w: linked directive cannot be changed", ErrInvalidInput)
	}
	directive, err := s.requireExecutingDirective(ctx, current.RelatedCode)
	if err != nil {
		return model.ExecutionConfirmation{}, err
	}
	current.Name = strings.TrimSpace(input.Name)
	current.Description = strings.TrimSpace(input.Description)
	current.Facility = directive.Facility
	current.Owner = strings.TrimSpace(input.Owner)
	current.Category = directive.Category
	current.RiskLevel = directive.RiskLevel
	current.MetricValue = input.MetricValue
	current.MetricUnit = strings.TrimSpace(input.MetricUnit)
	current.EffectiveAt = input.EffectiveAt.UTC()
	current.Evidence = strings.TrimSpace(input.Evidence)
	current.RelatedCode = directive.Code
	current.MeasuredGateState = strings.TrimSpace(input.MeasuredGateState)
	current.ObservedAt = input.ObservedAt.UTC()
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Update(txCtx, id, input.ExpectedVersion, &current); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "update", "ExecutionConfirmation", id, current.Status, current.Status, "updated business fields")
	}); err != nil {
		return model.ExecutionConfirmation{}, fmt.Errorf("update 执行确认: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *executionConfirmationService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, requestID string) (model.ExecutionConfirmation, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.ExecutionConfirmation{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.ExecutionConfirmationTransitions, current.Status, target) {
		return model.ExecutionConfirmation{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	before := current.Status
	now := time.Now().UTC()
	current.Status = target
	if target == "confirmed" {
		current.ConfirmedBy = actor
		current.ConfirmedAt = &now
	}
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = now

	var directiveTarget string
	if before == model.ExecutionConfirmationInitialStatus {
		// Both confirmation outcomes require the linked directive to still be
		// executing; the gate itself is never written by this transition. The
		// fresh re-read and field verification happen inside the transaction.
		if _, directiveErr := s.requireExecutingDirective(ctx, current.RelatedCode); directiveErr != nil {
			return model.ExecutionConfirmation{}, directiveErr
		}
		switch target {
		case "confirmed":
			directiveTarget = string(constants.DirectiveStateCompleted)
		case "failed":
			directiveTarget = string(constants.DirectiveStateAborted)
		}
	}

	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		// Re-read directive and gate inside the transaction so confirmation
		// reacts to the freshest gate state and cannot race a concurrent move.
		if directiveTarget == string(constants.DirectiveStateCompleted) {
			directive, directiveErr := s.directives.GetByCode(txCtx, current.RelatedCode)
			if directiveErr != nil {
				return fmt.Errorf("linked directive %q: %w", current.RelatedCode, directiveErr)
			}
			verification, verifyErr := s.evaluate(txCtx, current, directive)
			if verifyErr != nil {
				return verifyErr
			}
			if !verification.Passed {
				return fmt.Errorf("%w: 现场实测校验未通过：%s", ErrInvalidInput, strings.Join(verification.Reasons, "；"))
			}
		}
		if err := s.repository.Update(txCtx, id, input.ExpectedVersion, &current); err != nil {
			return err
		}
		if err := s.security.Audit(txCtx, actor, requestID, "transition", "ExecutionConfirmation", id, before, target, input.Reason); err != nil {
			return err
		}
		if directiveTarget == "" {
			return nil
		}
		directive, directiveErr := s.directives.GetByCode(txCtx, current.RelatedCode)
		if directiveErr != nil {
			return fmt.Errorf("linked directive %q: %w", current.RelatedCode, directiveErr)
		}
		directiveBefore := directive.Status
		if directiveBefore != directiveTarget {
			if !constants.CanTransition(constants.OperationDirectiveTransitions, directiveBefore, directiveTarget) {
				return fmt.Errorf("%w: directive %s cannot move from %s to %s", ErrInvalidTransition, directive.Code, directiveBefore, directiveTarget)
			}
			directive.Status = directiveTarget
			directive.Version++
			directive.UpdatedAt = now
			if err := s.directives.Update(txCtx, directive.ID, directive.Version-1, &directive); err != nil {
				return err
			}
			if err := s.security.Audit(txCtx, actor, requestID, "execution_outcome", "OperationDirective", directive.ID, directiveBefore, directiveTarget, input.Reason); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return model.ExecutionConfirmation{}, fmt.Errorf("transition 执行确认: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *executionConfirmationService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.Status != model.ExecutionConfirmationInitialStatus {
		return ErrImmutableState
	}
	return s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Delete(txCtx, id); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "delete", "ExecutionConfirmation", id, current.Status, "deleted", "soft deleted 执行确认")
	})
}

func (s *executionConfirmationService) requireExecutingDirective(ctx context.Context, code string) (model.OperationDirective, error) {
	directive, err := s.directives.GetByCode(ctx, code)
	if err != nil {
		return model.OperationDirective{}, fmt.Errorf("linked directive %q: %w", code, err)
	}
	if directive.Status != string(constants.DirectiveStateExecuting) {
		return model.OperationDirective{}, fmt.Errorf("%w: execution confirmation requires an executing directive", ErrInvalidInput)
	}
	return directive, nil
}

func (s *executionConfirmationService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

// attachVerification decorates pending and confirmed receipts with the live
// field check. It never mutates persisted state; callers only need read access.
func (s *executionConfirmationService) attachVerification(ctx context.Context, items []model.ExecutionConfirmation) error {
	directiveCodes := make([]string, 0, len(items))
	for i := range items {
		if items[i].Status == model.ExecutionConfirmationInitialStatus || items[i].Status == "confirmed" {
			directiveCodes = append(directiveCodes, items[i].RelatedCode)
		}
	}
	directiveByCode := map[string]model.OperationDirective{}
	if len(directiveCodes) > 0 {
		directives, err := s.directives.ListByCodes(ctx, directiveCodes)
		if err != nil {
			return fmt.Errorf("load linked directives: %w", err)
		}
		for _, directive := range directives {
			directiveByCode[directive.Code] = directive
		}
	}
	gateCodes := make([]string, 0)
	for _, directive := range directiveByCode {
		gateCodes = append(gateCodes, directive.RelatedCode)
	}
	gateByCode := map[string]model.GateUnit{}
	if len(gateCodes) > 0 {
		gates, err := s.gates.ListByCodes(ctx, gateCodes)
		if err != nil {
			return fmt.Errorf("load linked gates: %w", err)
		}
		for _, gate := range gates {
			gateByCode[gate.Code] = gate
		}
	}
	for i := range items {
		switch items[i].Status {
		case "confirmed":
			items[i].Verification = &model.ConfirmationVerification{Passed: true}
		case model.ExecutionConfirmationInitialStatus:
			directive, found := directiveByCode[items[i].RelatedCode]
			if !found {
				items[i].Verification = &model.ConfirmationVerification{Reasons: []string{fmt.Sprintf("关联指令 %s 不存在", items[i].RelatedCode)}}
				continue
			}
			if directive.Status != string(constants.DirectiveStateExecuting) {
				items[i].Verification = &model.ConfirmationVerification{Reasons: []string{fmt.Sprintf("关联指令 %s 当前为%s，需保持执行中", directive.Code, directiveStateLabel(directive.Status))}}
				continue
			}
			gate, found := gateByCode[directive.RelatedCode]
			if !found {
				items[i].Verification = &model.ConfirmationVerification{Reasons: []string{fmt.Sprintf("关联闸门 %s 不存在", directive.RelatedCode)}}
				continue
			}
			items[i].Verification = evaluateChecks(items[i], directive, gate)
		}
	}
	return nil
}

// evaluate runs the field check for a single pending receipt against a freshly
// loaded executing directive and its gate.
func (s *executionConfirmationService) evaluate(ctx context.Context, item model.ExecutionConfirmation, directive model.OperationDirective) (*model.ConfirmationVerification, error) {
	gate, err := s.gates.GetByCode(ctx, directive.RelatedCode)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return &model.ConfirmationVerification{Reasons: []string{fmt.Sprintf("关联闸门 %s 不存在", directive.RelatedCode)}}, nil
		}
		return nil, fmt.Errorf("linked gate %q: %w", directive.RelatedCode, err)
	}
	return evaluateChecks(item, directive, gate), nil
}

// evaluateChecks enforces the three on-site measurement rules: the measured
// gate position must equal the directive target, the observation must happen
// after execution started, and the gate must still be at that position now.
func evaluateChecks(item model.ExecutionConfirmation, directive model.OperationDirective, gate model.GateUnit) *model.ConfirmationVerification {
	reasons := make([]string, 0, 3)
	if item.MeasuredGateState != directive.GateState {
		reasons = append(reasons, fmt.Sprintf("实测闸位（%s）与指令目标闸位（%s）不一致", gateStateLabel(item.MeasuredGateState), gateStateLabel(directive.GateState)))
	}
	executionStart := directive.UpdatedAt.UTC()
	if directive.ExecutedAt != nil {
		executionStart = directive.ExecutedAt.UTC()
	}
	if !item.ObservedAt.UTC().After(executionStart) {
		reasons = append(reasons, fmt.Sprintf("观测时间（%s）必须晚于指令开始执行时间（%s）", formatCheckTime(item.ObservedAt), formatCheckTime(executionStart)))
	}
	if gate.Status != item.MeasuredGateState {
		reasons = append(reasons, fmt.Sprintf("当前闸门状态（%s）与实测闸位（%s）不一致", gateStateLabel(gate.Status), gateStateLabel(item.MeasuredGateState)))
	}
	return &model.ConfirmationVerification{Passed: len(reasons) == 0, Reasons: reasons}
}

func formatCheckTime(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04:05 UTC")
}

func gateStateLabel(state string) string {
	switch state {
	case string(constants.GateStateOpen):
		return "开启"
	case string(constants.GateStateClosed):
		return "关闭"
	case string(constants.GateStateMoving):
		return "动作中"
	case string(constants.GateStateLocked):
		return "闭锁"
	default:
		return state
	}
}

func directiveStateLabel(state string) string {
	switch state {
	case string(constants.DirectiveStateDraft):
		return "草稿"
	case string(constants.DirectiveStatePending):
		return "待处理"
	case string(constants.DirectiveStateApproved):
		return "已批准"
	case string(constants.DirectiveStateExecuting):
		return "执行中"
	case string(constants.DirectiveStateCompleted):
		return "已完成"
	case string(constants.DirectiveStateAborted):
		return "已中止"
	default:
		return state
	}
}

func validateExecutionConfirmationBusinessFields(code, name, facility, owner string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" {
		return ErrInvalidInput
	}
	return nil
}
