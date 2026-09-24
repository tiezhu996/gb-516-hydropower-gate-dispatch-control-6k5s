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
	return s.repository.List(ctx, query)
}

func (s *executionConfirmationService) Get(ctx context.Context, id uint) (model.ExecutionConfirmation, error) {
	return s.repository.Get(ctx, id)
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
		RelatedCode: directive.Code,
		MeasuredGateState: strings.TrimSpace(input.MeasuredGateState), ObservedAt: ptrTime(input.ObservedAt.UTC()),
	}
	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Create(txCtx, &item); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "create", "ExecutionConfirmation", item.ID, "", item.Status, "created 执行确认")
	}); err != nil {
		return model.ExecutionConfirmation{}, fmt.Errorf("create 执行确认: %w", err)
	}
	return item, nil
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
	current.ObservedAt = ptrTime(input.ObservedAt.UTC())
	// Editing the receipt clears the previous verification outcome so a stale
	// failure reason is never presented against corrected evidence.
	current.VerifyStatus = ""
	current.VerifyDetail = ""
	current.VerifiedAt = nil
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
	return s.repository.Get(ctx, id)
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

	var directive model.OperationDirective
	var gate model.GateUnit
	var directiveTarget, gateTarget string
	var verifyFailure string
	if before == model.ExecutionConfirmationInitialStatus {
		directive, err = s.requireExecutingDirective(ctx, current.RelatedCode)
		if err != nil {
			return model.ExecutionConfirmation{}, err
		}
		gate, err = s.gates.GetByCode(ctx, directive.RelatedCode)
		if err != nil {
			return model.ExecutionConfirmation{}, fmt.Errorf("linked gate %q: %w", directive.RelatedCode, err)
		}
		if target == "failed" {
			directiveTarget, gateTarget = string(constants.DirectiveStateAborted), string(constants.GateStateLocked)
			if gate.Status != gateTarget && !constants.CanTransition(constants.GateUnitTransitions, gate.Status, gateTarget) {
				return model.ExecutionConfirmation{}, fmt.Errorf("%w: gate %s cannot move from %s to %s", ErrInvalidTransition, gate.Code, gate.Status, gateTarget)
			}
		} else {
			// Confirmation never drives the gate: the gate must already be at
			// the target position. Re-read both directive and gate inside the
			// verification checks below, inside the committing transaction.
			directiveTarget = string(constants.DirectiveStateCompleted)
			verifyFailure = verifyConfirmation(ctx, current, &directive, &gate, s)
		}
	}

	if verifyFailure != "" {
		// Any failed check keeps the receipt pending and the directive
		// executing. Persist the detailed verification outcome so the page can
		// explain the reason; the gate is intentionally never written here.
		current.VerifyStatus = "failed"
		current.VerifyDetail = verifyFailure
		current.VerifiedAt = &now
		current.Version = input.ExpectedVersion + 1
		current.UpdatedAt = now
		if txErr := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
			if err := s.repository.Update(txCtx, id, input.ExpectedVersion, &current); err != nil {
				return err
			}
			return s.security.Audit(txCtx, actor, requestID, "verification_failed", "ExecutionConfirmation", id, before, before, verifyFailure)
		}); txErr != nil {
			return model.ExecutionConfirmation{}, fmt.Errorf("record verification failure: %w", txErr)
		}
		stored, getErr := s.repository.Get(ctx, id)
		if getErr != nil {
			return model.ExecutionConfirmation{}, getErr
		}
		return stored, fmt.Errorf("%w: %s", ErrInvalidInput, verifyFailure)
	}

	current.Status = target
	if target == "confirmed" {
		current.ConfirmedBy = actor
		current.ConfirmedAt = &now
		current.VerifyStatus = "passed"
		current.VerifyDetail = "实测闸位与指令目标一致，观测时间晚于开始执行，当前闸门状态与实测值相同"
		current.VerifiedAt = &now
	}
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = now

	if err := s.security.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repository.Update(txCtx, id, input.ExpectedVersion, &current); err != nil {
			return err
		}
		if err := s.security.Audit(txCtx, actor, requestID, "transition", "ExecutionConfirmation", id, before, target, input.Reason); err != nil {
			return err
		}
		if directiveTarget == "" {
			return nil
		}
		// Re-read the directive and gate inside the transaction so a concurrent
		// state change cannot be overwritten by stale data loaded beforehand.
		freshDirective, directiveErr := s.directives.Get(txCtx, directive.ID)
		if directiveErr != nil {
			return directiveErr
		}
		if freshDirective.Status != string(constants.DirectiveStateExecuting) {
			return fmt.Errorf("%w: directive %s is no longer executing", ErrInvalidTransition, directive.Code)
		}
		freshGate, gateErr := s.gates.Get(txCtx, gate.ID)
		if gateErr != nil {
			return gateErr
		}
		directiveBefore := freshDirective.Status
		if target == "confirmed" {
			// The gate must already equal the measured value. The confirmation
			// interface never writes it.
			if freshGate.Status != current.MeasuredGateState {
				return fmt.Errorf("%w: gate %s currently reads %s, measured value is %s",
					ErrInvalidInput, gate.Code, freshGate.Status, current.MeasuredGateState)
			}
		} else if freshGate.Status != gateTarget &&
			!constants.CanTransition(constants.GateUnitTransitions, freshGate.Status, gateTarget) {
			return fmt.Errorf("%w: gate %s cannot move from %s to %s",
				ErrInvalidTransition, gate.Code, freshGate.Status, gateTarget)
		}
		directive.Status = directiveTarget
		directive.Version = freshDirective.Version + 1
		directive.UpdatedAt = now
		if err := s.directives.Update(txCtx, directive.ID, freshDirective.Version, &directive); err != nil {
			return err
		}
		if err := s.security.Audit(txCtx, actor, requestID, "execution_outcome", "OperationDirective", directive.ID, directiveBefore, directiveTarget, input.Reason); err != nil {
			return err
		}
		if gateTarget == "" || freshGate.Status == gateTarget {
			return nil
		}
		gateBefore := freshGate.Status
		freshGate.Status = gateTarget
		freshGate.Version++
		freshGate.UpdatedAt = now
		if err := s.gates.Update(txCtx, gate.ID, freshGate.Version-1, &freshGate); err != nil {
			return err
		}
		return s.security.Audit(txCtx, actor, requestID, "execution_outcome", "GateUnit", gate.ID, gateBefore, gateTarget, input.Reason)
	}); err != nil {
		return model.ExecutionConfirmation{}, fmt.Errorf("transition 执行确认: %w", err)
	}
	return s.repository.Get(ctx, id)
}

// verifyConfirmation enforces the three on-site evidence rules. Every check
// re-reads current state rather than trusting stale caller data. A non-empty
// result is the human-readable reason the directive must not be completed.
func verifyConfirmation(ctx context.Context, receipt model.ExecutionConfirmation, directive *model.OperationDirective, gate *model.GateUnit, s *executionConfirmationService) string {
	freshDirective, err := s.directives.GetByCode(ctx, receipt.RelatedCode)
	if err != nil {
		return fmt.Sprintf("无法重新读取关联指令 %s：%v", receipt.RelatedCode, err)
	}
	*directive = freshDirective
	if freshDirective.Status != string(constants.DirectiveStateExecuting) {
		return fmt.Sprintf("关联指令 %s 当前状态为 %s，不是执行中，不能完成", freshDirective.Code, freshDirective.Status)
	}
	if freshDirective.ExecutedAt == nil {
		return fmt.Sprintf("关联指令 %s 缺少开始执行时间，无法校验观测时序", freshDirective.Code)
	}
	freshGate, err := s.gates.GetByCode(ctx, freshDirective.RelatedCode)
	if err != nil {
		return fmt.Sprintf("无法重新读取闸门 %s：%v", freshDirective.RelatedCode, err)
	}
	*gate = freshGate

	measured := strings.TrimSpace(receipt.MeasuredGateState)
	if measured == "" || receipt.ObservedAt == nil || receipt.ObservedAt.IsZero() {
		return "回执缺少现场实测闸位或观测时间，请补全后再确认"
	}
	if measured != freshDirective.GateState {
		return fmt.Sprintf("实测闸位 %s 与指令目标 %s 不一致", stateLabel(measured), stateLabel(freshDirective.GateState))
	}
	if !receipt.ObservedAt.UTC().After(freshDirective.ExecutedAt.UTC()) {
		return fmt.Sprintf("观测时间 %s 不晚于开始执行时间 %s",
			receipt.ObservedAt.UTC().Format(time.RFC3339), freshDirective.ExecutedAt.UTC().Format(time.RFC3339))
	}
	if freshGate.Status != measured {
		return fmt.Sprintf("闸门 %s 当前状态为 %s，与实测值 %s 不一致", freshGate.Code, stateLabel(freshGate.Status), stateLabel(measured))
	}
	return ""
}

func stateLabel(state string) string {
	switch state {
	case "open":
		return "开启"
	case "closed":
		return "关闭"
	case "moving":
		return "动作中"
	case "locked":
		return "闭锁"
	default:
		return state
	}
}

func ptrTime(value time.Time) *time.Time {
	return &value
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

func validateExecutionConfirmationBusinessFields(code, name, facility, owner string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" {
		return ErrInvalidInput
	}
	return nil
}
