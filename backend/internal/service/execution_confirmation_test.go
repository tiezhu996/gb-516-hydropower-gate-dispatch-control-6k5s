package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/config"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/dto"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/model"
	"github.com/blueship581/hydropower-gate-dispatch-control/backend/internal/repository"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newExecutionWorkflow(t *testing.T) (ExecutionConfirmationService, OperationDirectiveService, repository.GateUnitRepository, repository.OperationDirectiveRepository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.GateUnit{}, &model.OperationDirective{}, &model.DirectiveApproval{}, &model.ExecutionConfirmation{}, &model.AuditLog{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	gateRepo := repository.NewGateUnitRepository(db)
	directiveRepo := repository.NewOperationDirectiveRepository(db)
	confirmationRepo := repository.NewExecutionConfirmationRepository(db)
	security := NewSecurityService(repository.NewSecurityRepository(db), config.Config{})
	directives := NewOperationDirectiveService(directiveRepo, gateRepo, security)
	confirmations := NewExecutionConfirmationService(confirmationRepo, directiveRepo, gateRepo, security)
	gate := model.GateUnit{BaseModel: model.BaseModel{Code: "GU-FLOW", Name: "泄洪闸", Status: "closed", Version: 1}, Facility: "主坝", Owner: "运行一组"}
	if err := gateRepo.Create(context.Background(), &gate); err != nil {
		t.Fatalf("create gate: %v", err)
	}
	return confirmations, directives, gateRepo, directiveRepo, db
}

func prepareExecutingDirective(t *testing.T, directives OperationDirectiveService) model.OperationDirective {
	t.Helper()
	ctx := context.Background()
	created, err := directives.Create(ctx, dto.CreateOperationDirective{
		Code: "OD-FLOW", Name: "开启泄洪闸", Facility: "主坝", Owner: "运行一组",
		Category: "泄洪", RiskLevel: "high", MetricValue: 35, MetricUnit: "%",
		EffectiveAt: time.Now().UTC().Add(time.Hour), Evidence: "水位与通信核对完成",
		RelatedCode: "GU-FLOW", GateState: "open",
	}, "operator", "req-create")
	if err != nil {
		t.Fatalf("create directive: %v", err)
	}
	submitted, err := directives.Transition(ctx, created.ID, dto.TransitionRequest{Status: "pending", ExpectedVersion: created.Version, Reason: "提交复核"}, "operator", model.RoleOperator, "req-submit")
	if err != nil {
		t.Fatalf("submit directive: %v", err)
	}
	approved, err := directives.Transition(ctx, submitted.ID, dto.TransitionRequest{Status: "approved", ExpectedVersion: submitted.Version, Reason: "独立复核通过"}, "reviewer", model.RoleReviewer, "req-approve")
	if err != nil {
		t.Fatalf("approve directive: %v", err)
	}
	executing, err := directives.Transition(ctx, approved.ID, dto.TransitionRequest{Status: "executing", ExpectedVersion: approved.Version, Reason: "现场开始执行"}, "operator", model.RoleOperator, "req-execute")
	if err != nil {
		t.Fatalf("execute directive: %v", err)
	}
	return executing
}

// moveGateToTarget simulates the physical gate reaching the directive target
// while the receipt is pending. The confirmation API itself must never do this.
func moveGateToTarget(t *testing.T, gates repository.GateUnitRepository, state string) model.GateUnit {
	t.Helper()
	gate, err := gates.GetByCode(context.Background(), "GU-FLOW")
	if err != nil {
		t.Fatalf("load gate: %v", err)
	}
	before := gate.Version
	gate.Status = state
	gate.Version++
	gate.UpdatedAt = time.Now().UTC()
	if err := gates.Update(context.Background(), gate.ID, before, &gate); err != nil {
		t.Fatalf("move gate to %s: %v", state, err)
	}
	return gate
}

func createPendingConfirmation(t *testing.T, confirmations ExecutionConfirmationService, measuredState string, observedAt time.Time) model.ExecutionConfirmation {
	t.Helper()
	item, err := confirmations.Create(context.Background(), dto.CreateExecutionConfirmation{
		Code: "EC-FLOW", Name: "现场执行回执", Facility: "主坝", Owner: "现场操作员",
		Category: "执行", RiskLevel: "high", MetricValue: 35, MetricUnit: "%",
		EffectiveAt: time.Now().UTC(), Evidence: "开度反馈与视频记录一致", RelatedCode: "OD-FLOW",
		MeasuredGateState: measuredState, ObservedAt: observedAt,
	}, "operator", "req-confirm-create")
	if err != nil {
		t.Fatalf("create confirmation: %v", err)
	}
	return item
}

func TestExecutionConfirmationCompletesDirectiveOnlyWhenMeasurementsMatch(t *testing.T) {
	confirmations, directives, gates, _, db := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	gate, _ := gates.GetByCode(context.Background(), "GU-FLOW")
	if gate.Status != "moving" {
		t.Fatalf("gate should enter moving when directive executes, got %s", gate.Status)
	}
	if executing.ExecutedAt == nil {
		t.Fatal("directive should record when execution started")
	}
	pending := createPendingConfirmation(t, confirmations, "open", time.Now().UTC())

	// Gate still moving: confirmation must be rejected and nothing may change.
	if _, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "闸门尚未到位",
	}, "operator", "req-confirm-reject"); err == nil {
		t.Fatal("confirmation must fail while the gate has not reached the measured state")
	}
	storedConfirmation, _ := confirmations.Get(context.Background(), pending.ID)
	storedDirective, _ := directives.Get(context.Background(), executing.ID)
	storedGate, _ := gates.GetByCode(context.Background(), "GU-FLOW")
	if storedConfirmation.Status != "pending" || storedDirective.Status != "executing" || storedGate.Status != "moving" {
		t.Fatalf("rejected confirmation must keep receipt pending and directive executing: receipt=%s directive=%s gate=%s",
			storedConfirmation.Status, storedDirective.Status, storedGate.Status)
	}

	// Physical gate reaches the target before confirmation.
	moveGateToTarget(t, gates, "open")
	confirmed, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "目标开度和现场反馈一致",
	}, "operator", "req-confirm")
	if err != nil {
		t.Fatalf("confirm execution after gate reaches target: %v", err)
	}
	updatedDirective, _ := directives.Get(context.Background(), executing.ID)
	updatedGate, _ := gates.GetByCode(context.Background(), "GU-FLOW")
	if confirmed.Status != "confirmed" || updatedDirective.Status != "completed" || updatedGate.Status != "open" {
		t.Fatalf("workflow not completed: confirmation=%s directive=%s gate=%s", confirmed.Status, updatedDirective.Status, updatedGate.Status)
	}
	if confirmed.MeasuredGateState != "open" || confirmed.ObservedAt.IsZero() {
		t.Fatalf("measured fields not preserved: %#v", confirmed)
	}
	if confirmed.Verification == nil || !confirmed.Verification.Passed {
		t.Fatalf("confirmed receipt should expose passing verification, got %#v", confirmed.Verification)
	}
	// Receipt transition audit + directive outcome audit; the gate is untouched.
	var linkedAudits int64
	if err := db.Model(&model.AuditLog{}).Where("request_id = ?", "req-confirm").Count(&linkedAudits).Error; err != nil || linkedAudits != 2 {
		t.Fatalf("expected two linked audit events, count=%d err=%v", linkedAudits, err)
	}
}

func TestExecutionConfirmationRejectsMismatchedMeasuredGateState(t *testing.T) {
	confirmations, directives, gates, _, _ := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	moveGateToTarget(t, gates, "closed")
	pending := createPendingConfirmation(t, confirmations, "closed", time.Now().UTC())
	_, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "误填目标闸位",
	}, "operator", "req-measured-mismatch")
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "实测闸位") {
		t.Fatalf("expected measured-vs-target business error, got %v", err)
	}
	stored, _ := confirmations.Get(context.Background(), pending.ID)
	if stored.Status != "pending" {
		t.Fatalf("receipt must remain pending, got %s", stored.Status)
	}
	directive, _ := directives.Get(context.Background(), executing.ID)
	if directive.Status != "executing" {
		t.Fatalf("directive must remain executing, got %s", directive.Status)
	}
}

func TestExecutionConfirmationRejectsObservationBeforeExecutionStart(t *testing.T) {
	confirmations, directives, gates, _, _ := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	moveGateToTarget(t, gates, "open")
	pending := createPendingConfirmation(t, confirmations, "open", executing.ExecutedAt.Add(-time.Minute))
	_, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "观测时间早于执行",
	}, "operator", "req-observation-time")
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "观测时间") {
		t.Fatalf("expected observation-time business error, got %v", err)
	}
	if stored, _ := confirmations.Get(context.Background(), pending.ID); stored.Status != "pending" {
		t.Fatalf("receipt must remain pending, got %s", stored.Status)
	}
}

func TestExecutionConfirmationRejectsWhenGateMovedAwayAfterMeasurement(t *testing.T) {
	confirmations, directives, gates, _, _ := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	pending := createPendingConfirmation(t, confirmations, "open", time.Now().UTC())
	// Gate reached open when measured, but moved again before confirmation.
	moveGateToTarget(t, gates, "open")
	moveGateToTarget(t, gates, "moving")
	_, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "闸门已偏离实测值",
	}, "operator", "req-gate-drift")
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "当前闸门状态") {
		t.Fatalf("expected current-gate-state business error, got %v", err)
	}
	stored, _ := confirmations.Get(context.Background(), pending.ID)
	directive, _ := directives.Get(context.Background(), executing.ID)
	gate, _ := gates.GetByCode(context.Background(), "GU-FLOW")
	if stored.Status != "pending" || directive.Status != "executing" || gate.Status != "moving" {
		t.Fatalf("all state must stay in progress: receipt=%s directive=%s gate=%s", stored.Status, directive.Status, gate.Status)
	}
}

func TestPendingConfirmationExposesLiveVerificationOnRead(t *testing.T) {
	confirmations, directives, gates, _, _ := newExecutionWorkflow(t)
	prepareExecutingDirective(t, directives)
	pending := createPendingConfirmation(t, confirmations, "open", time.Now().UTC())

	page, err := confirmations.List(context.Background(), dto.PageQuery{})
	if err != nil {
		t.Fatalf("list confirmations: %v", err)
	}
	var listed *model.ExecutionConfirmation
	for i := range page.Items {
		if page.Items[i].ID == pending.ID {
			listed = &page.Items[i]
		}
	}
	if listed == nil || listed.Verification == nil || listed.Verification.Passed {
		t.Fatalf("pending receipt against a moving gate should show failing verification, got %#v", listed)
	}
	if len(listed.Verification.Reasons) != 1 || !strings.Contains(listed.Verification.Reasons[0], "当前闸门状态") {
		t.Fatalf("unexpected verification reasons: %#v", listed.Verification.Reasons)
	}
	loaded, _ := confirmations.Get(context.Background(), pending.ID)
	if loaded.Verification == nil {
		t.Fatal("get should also attach verification")
	}

	moveGateToTarget(t, gates, "open")
	loaded, _ = confirmations.Get(context.Background(), pending.ID)
	if loaded.Verification == nil || !loaded.Verification.Passed {
		t.Fatalf("verification should pass once the gate matches the measurement, got %#v", loaded.Verification)
	}
}

func TestFailedConfirmationAbortsDirectiveWithoutWritingGate(t *testing.T) {
	confirmations, directives, gates, _, _ := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	pending := createPendingConfirmation(t, confirmations, "closed", time.Now().UTC())
	failed, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "failed", ExpectedVersion: pending.Version, Reason: "现场无法继续执行",
	}, "operator", "req-failed")
	if err != nil {
		t.Fatalf("mark confirmation failed: %v", err)
	}
	directive, _ := directives.Get(context.Background(), executing.ID)
	gate, _ := gates.GetByCode(context.Background(), "GU-FLOW")
	if failed.Status != "failed" || directive.Status != "aborted" {
		t.Fatalf("failed receipt should abort directive: receipt=%s directive=%s", failed.Status, directive.Status)
	}
	if gate.Status != "moving" {
		t.Fatalf("confirmation must not rewrite the gate, got %s", gate.Status)
	}
}

func TestExecutionConfirmationRollsBackAllStateWhenAuditFails(t *testing.T) {
	confirmations, directives, gates, _, db := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	pending := createPendingConfirmation(t, confirmations, "open", time.Now().UTC())
	moveGateToTarget(t, gates, "open")
	if err := db.Migrator().DropTable(&model.AuditLog{}); err != nil {
		t.Fatalf("drop audit table: %v", err)
	}
	_, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "审计失败必须整体回滚",
	}, "operator", "req-rollback")
	if err == nil {
		t.Fatal("transition should fail without audit persistence")
	}
	storedConfirmation, _ := confirmations.Get(context.Background(), pending.ID)
	storedDirective, _ := directives.Get(context.Background(), executing.ID)
	storedGate, _ := gates.GetByCode(context.Background(), "GU-FLOW")
	if storedConfirmation.Status != "pending" || storedDirective.Status != "executing" || storedGate.Status != "open" {
		t.Fatalf("partial state persisted: confirmation=%s directive=%s gate=%s", storedConfirmation.Status, storedDirective.Status, storedGate.Status)
	}
}
