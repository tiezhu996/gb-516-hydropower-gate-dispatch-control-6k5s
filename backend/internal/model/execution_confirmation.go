package model

import "time"

// ExecutionConfirmation models 执行确认 as an independently versioned aggregate. The fields
// cover ownership, operational context, evidence and measured risk so later
// changes naturally span persistence, service and UI layers.
type ExecutionConfirmation struct {
	BaseModel
	Facility    string     `json:"facility" gorm:"size:120;index"`
	Owner       string     `json:"owner" gorm:"size:120;index"`
	Category    string     `json:"category" gorm:"size:80;index"`
	RiskLevel   string     `json:"riskLevel" gorm:"size:32;index"`
	MetricValue float64    `json:"metricValue"`
	MetricUnit  string     `json:"metricUnit" gorm:"size:24"`
	EffectiveAt time.Time  `json:"effectiveAt"`
	Evidence    string     `json:"evidence" gorm:"size:2000"`
	RelatedCode string     `json:"relatedCode" gorm:"size:64;uniqueIndex;not null"`
	ConfirmedBy string     `json:"confirmedBy" gorm:"size:80;index"`
	ConfirmedAt *time.Time `json:"confirmedAt"`
	// MeasuredGateState is the gate position measured on site; ObservedAt is
	// when that measurement was taken. A directive completes only when the
	// measurement equals the directive target, was observed after execution
	// started, and still matches the gate read back at confirmation time.
	MeasuredGateState string     `json:"measuredGateState" gorm:"size:32;index"`
	ObservedAt        *time.Time `json:"observedAt"`
	// VerifyStatus/VerifyDetail persist the latest confirmation-time
	// verification. A failed verification keeps the receipt pending and the
	// directive executing so the operator can correct the receipt and retry.
	VerifyStatus string     `json:"verifyStatus" gorm:"size:16;index"`
	VerifyDetail string     `json:"verifyDetail" gorm:"size:500"`
	VerifiedAt   *time.Time `json:"verifiedAt"`
}

func (item *ExecutionConfirmation) GetBase() *BaseModel { return &item.BaseModel }

func (item ExecutionConfirmation) TableName() string { return "execution_confirmations" }

var ExecutionConfirmationInitialStatus = "pending"
