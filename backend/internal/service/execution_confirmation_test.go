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

func newExecutionWorkflow(t *testing.T) (ExecutionConfirmationService, OperationDirectiveService, GateUnitService, repository.GateUnitRepository, repository.OperationDirectiveRepository, *gorm.DB) {
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
	gates := NewGateUnitService(gateRepo, repository.NewReservoirRepository(db), security)
	directives := NewOperationDirectiveService(directiveRepo, gateRepo, security)
	confirmations := NewExecutionConfirmationService(confirmationRepo, directiveRepo, gateRepo, security)
	gate := model.GateUnit{BaseModel: model.BaseModel{Code: "GU-FLOW", Name: "泄洪闸", Status: "closed", Version: 1}, Facility: "主坝", Owner: "运行一组"}
	if err := gateRepo.Create(context.Background(), &gate); err != nil {
		t.Fatalf("create gate: %v", err)
	}
	return confirmations, directives, gates, gateRepo, directiveRepo, db
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

func createPendingConfirmation(t *testing.T, confirmations ExecutionConfirmationService, measured string, observedAt time.Time) model.ExecutionConfirmation {
	t.Helper()
	item, err := confirmations.Create(context.Background(), dto.CreateExecutionConfirmation{
		Code: "EC-FLOW", Name: "现场执行回执", Facility: "主坝", Owner: "现场操作员",
		Category: "执行", RiskLevel: "high", MetricValue: 35, MetricUnit: "%",
		EffectiveAt: time.Now().UTC(), Evidence: "开度反馈与视频记录一致", RelatedCode: "OD-FLOW",
		MeasuredGateState: measured, ObservedAt: observedAt,
	}, "operator", "req-confirm-create")
	if err != nil {
		t.Fatalf("create confirmation: %v", err)
	}
	return item
}

// settleGate moves the linked gate through moving into the target position the
// way field equipment would report it back. The confirmation API itself must
// never write the gate.
func settleGate(t *testing.T, gates GateUnitService, gateRepo repository.GateUnitRepository, target string) {
	t.Helper()
	ctx := context.Background()
	moving, err := gateRepo.GetByCode(ctx, "GU-FLOW")
	if err != nil {
		t.Fatalf("load gate: %v", err)
	}
	if moving.Status != "moving" {
		return
	}
	if _, err := gates.Transition(ctx, moving.ID, dto.TransitionRequest{
		Status: target, ExpectedVersion: moving.Version, Reason: "现场设备反馈闸位落定",
	}, "operator", "req-gate-settle"); err != nil {
		t.Fatalf("settle gate to %s: %v", target, err)
	}
}

func TestExecutionConfirmationCompletesDirectiveAndGateAtomically(t *testing.T) {
	confirmations, directives, gates, gateRepo, _, db := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	gate, _ := gateRepo.GetByCode(context.Background(), "GU-FLOW")
	if gate.Status != "moving" {
		t.Fatalf("gate should enter moving when directive executes, got %s", gate.Status)
	}
	// Field equipment reports the gate settled at the directive target.
	settleGate(t, gates, gateRepo, "open")
	observedAt := executing.ExecutedAt.Add(time.Minute)
	pending := createPendingConfirmation(t, confirmations, "open", observedAt)
	confirmed, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "目标开度和现场反馈一致",
	}, "operator", "req-confirm")
	if err != nil {
		t.Fatalf("confirm execution: %v", err)
	}
	updatedDirective, _ := directives.Get(context.Background(), executing.ID)
	updatedGate, _ := gateRepo.GetByCode(context.Background(), "GU-FLOW")
	if confirmed.Status != "confirmed" || updatedDirective.Status != "completed" || updatedGate.Status != "open" {
		t.Fatalf("workflow not completed atomically: confirmation=%s directive=%s gate=%s", confirmed.Status, updatedDirective.Status, updatedGate.Status)
	}
	if confirmed.MeasuredGateState != "open" || confirmed.ObservedAt == nil || confirmed.VerifyStatus != "passed" {
		t.Fatalf("confirmed receipt lost field evidence: %#v", confirmed)
	}
	// Confirmation writes the receipt and directive only — never the gate: only
	// two audit events carry the confirmation request id (the gate settlement
	// was reported separately as req-gate-settle).
	var linkedAudits int64
	if err := db.Model(&model.AuditLog{}).Where("request_id = ?", "req-confirm").Count(&linkedAudits).Error; err != nil || linkedAudits != 2 {
		t.Fatalf("expected two linked audit events without gate rewrite, count=%d err=%v", linkedAudits, err)
	}
}

func TestExecutionConfirmationRejectsMeasuredValueMismatch(t *testing.T) {
	confirmations, directives, gates, gateRepo, _, _ := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	settleGate(t, gates, gateRepo, "open")
	pending := createPendingConfirmation(t, confirmations, "closed", executing.ExecutedAt.Add(time.Minute))
	_, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "现场实测与目标不符",
	}, "operator", "req-confirm-mismatch")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected business rule error, got %v", err)
	}
	storedConfirmation, _ := confirmations.Get(context.Background(), pending.ID)
	storedDirective, _ := directives.Get(context.Background(), executing.ID)
	storedGate, _ := gateRepo.GetByCode(context.Background(), "GU-FLOW")
	if storedConfirmation.Status != "pending" || storedConfirmation.Version != pending.Version+1 {
		t.Fatalf("receipt must stay pending with bumped version, got %#v", storedConfirmation)
	}
	if storedConfirmation.VerifyStatus != "failed" || !strings.Contains(storedConfirmation.VerifyDetail, "指令目标") {
		t.Fatalf("verification reason not recorded, got %#v", storedConfirmation)
	}
	if storedDirective.Status != "executing" || storedGate.Status != "open" {
		t.Fatalf("directive and gate must stay untouched: directive=%s gate=%s", storedDirective.Status, storedGate.Status)
	}
	// After correcting the receipt, confirmation succeeds with the fresh gate read.
	observedAt := executing.ExecutedAt.Add(2 * time.Minute)
	corrected, err := confirmations.Update(context.Background(), pending.ID, dto.UpdateExecutionConfirmation{
		ExpectedVersion: storedConfirmation.Version, Name: storedConfirmation.Name, Facility: storedConfirmation.Facility,
		Owner: storedConfirmation.Owner, Category: storedConfirmation.Category, RiskLevel: storedConfirmation.RiskLevel,
		MetricValue: storedConfirmation.MetricValue, MetricUnit: storedConfirmation.MetricUnit, EffectiveAt: storedConfirmation.EffectiveAt,
		Evidence: storedConfirmation.Evidence, RelatedCode: storedConfirmation.RelatedCode,
		MeasuredGateState: "open", ObservedAt: observedAt,
	}, "operator", "req-confirm-fix")
	if err != nil {
		t.Fatalf("correct receipt: %v", err)
	}
	if corrected.VerifyStatus != "" {
		t.Fatalf("editing the receipt must clear the stale verification result")
	}
	if _, err := confirmations.Transition(context.Background(), corrected.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: corrected.Version, Reason: "补测后与目标一致",
	}, "operator", "req-confirm-retry"); err != nil {
		t.Fatalf("corrected receipt should confirm: %v", err)
	}
}

func TestExecutionConfirmationRejectsObservationBeforeExecutionStart(t *testing.T) {
	confirmations, directives, gates, gateRepo, _, _ := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	settleGate(t, gates, gateRepo, "open")
	pending := createPendingConfirmation(t, confirmations, "open", executing.ExecutedAt.Add(-time.Minute))
	_, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "观测时间早于执行开始",
	}, "operator", "req-confirm-time")
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "开始执行") {
		t.Fatalf("expected observation timing error, got %v", err)
	}
	storedConfirmation, _ := confirmations.Get(context.Background(), pending.ID)
	storedDirective, _ := directives.Get(context.Background(), executing.ID)
	storedGate, _ := gateRepo.GetByCode(context.Background(), "GU-FLOW")
	if storedConfirmation.Status != "pending" || storedConfirmation.VerifyStatus != "failed" ||
		storedDirective.Status != "executing" || storedGate.Status != "open" {
		t.Fatalf("state must be retained on timing failure: receipt=%#v directive=%s gate=%s",
			storedConfirmation, storedDirective.Status, storedGate.Status)
	}
}

func TestExecutionConfirmationRejectsGateReadingDifferentFromMeasured(t *testing.T) {
	confirmations, directives, _, gateRepo, _, _ := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	// Gate is still moving while the receipt claims the target position.
	pending := createPendingConfirmation(t, confirmations, "open", executing.ExecutedAt.Add(time.Minute))
	_, err := confirmations.Transition(context.Background(), pending.ID, dto.TransitionRequest{
		Status: "confirmed", ExpectedVersion: pending.Version, Reason: "闸门仍在动作中",
	}, "operator", "req-confirm-gate")
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "当前状态") {
		t.Fatalf("expected current gate mismatch error, got %v", err)
	}
	storedConfirmation, _ := confirmations.Get(context.Background(), pending.ID)
	storedDirective, _ := directives.Get(context.Background(), executing.ID)
	storedGate, _ := gateRepo.GetByCode(context.Background(), "GU-FLOW")
	if storedConfirmation.Status != "pending" || storedDirective.Status != "executing" || storedGate.Status != "moving" {
		t.Fatalf("state must be retained: receipt=%s directive=%s gate=%s",
			storedConfirmation.Status, storedDirective.Status, storedGate.Status)
	}
	if !strings.Contains(storedConfirmation.VerifyDetail, "实测值") {
		t.Fatalf("verification detail should quote measured mismatch, got %q", storedConfirmation.VerifyDetail)
	}
}

func TestExecutionConfirmationRollsBackAllStateWhenAuditFails(t *testing.T) {
	confirmations, directives, gates, gateRepo, _, db := newExecutionWorkflow(t)
	executing := prepareExecutingDirective(t, directives)
	settleGate(t, gates, gateRepo, "open")
	pending := createPendingConfirmation(t, confirmations, "open", executing.ExecutedAt.Add(time.Minute))
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
	storedGate, _ := gateRepo.GetByCode(context.Background(), "GU-FLOW")
	if storedConfirmation.Status != "pending" || storedConfirmation.Version != pending.Version || storedDirective.Status != "executing" || storedGate.Status != "open" {
		t.Fatalf("partial state persisted: confirmation=%s(v%d) directive=%s gate=%s",
			storedConfirmation.Status, storedConfirmation.Version, storedDirective.Status, storedGate.Status)
	}
}
