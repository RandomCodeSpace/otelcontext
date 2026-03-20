package graphrag

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// Investigation is a persisted record of an automated error investigation.
type Investigation struct {
	ID               string          `gorm:"primaryKey;size:64" json:"id"`
	CreatedAt        time.Time       `json:"created_at"`
	Status           string          `gorm:"size:20" json:"status"`    // detected, triaged, resolved
	Severity         string          `gorm:"size:20" json:"severity"`  // critical, warning, info
	TriggerService   string          `gorm:"size:255;index" json:"trigger_service"`
	TriggerOperation string          `gorm:"size:255" json:"trigger_operation"`
	ErrorMessage     string          `gorm:"type:text" json:"error_message"`
	RootService      string          `gorm:"size:255" json:"root_service"`
	RootOperation    string          `gorm:"size:255" json:"root_operation"`
	CausalChain      json.RawMessage `gorm:"type:text" json:"causal_chain"`
	TraceIDs         json.RawMessage `gorm:"type:text" json:"trace_ids"`
	ErrorLogs        json.RawMessage `gorm:"type:text" json:"error_logs"`
	AnomalousMetrics json.RawMessage `gorm:"type:text" json:"anomalous_metrics"`
	AffectedServices json.RawMessage `gorm:"type:text" json:"affected_services"`
	SpanChain        json.RawMessage `gorm:"type:text" json:"span_chain"`
}

// TableName overrides GORM's default table name.
func (Investigation) TableName() string {
	return "investigations"
}

// AutoMigrateGraphRAG runs GORM auto-migration for GraphRAG models.
func AutoMigrateGraphRAG(db *gorm.DB) error {
	return db.AutoMigrate(&Investigation{}, &GraphSnapshot{})
}

// PersistInvestigation saves an investigation record from an error chain analysis.
func (g *GraphRAG) PersistInvestigation(triggerService string, chains []ErrorChainResult, anomalies []*AnomalyNode) {
	if len(chains) == 0 {
		return
	}

	firstChain := chains[0]
	if firstChain.RootCause == nil {
		return
	}

	id := fmt.Sprintf("inv_%d", time.Now().UnixNano())

	severity := "warning"
	if len(anomalies) > 0 {
		for _, a := range anomalies {
			if a.Severity == SeverityCritical {
				severity = "critical"
				break
			}
		}
	}

	// Collect trace IDs
	var traceIDs []string
	for _, c := range chains {
		traceIDs = append(traceIDs, c.TraceID)
	}

	// Build causal chain
	type causalStep struct {
		Service   string `json:"service"`
		Operation string `json:"operation"`
		SpanID    string `json:"span_id"`
		IsError   bool   `json:"is_error"`
	}
	var causal []causalStep
	for _, s := range firstChain.SpanChain {
		causal = append(causal, causalStep{
			Service:   s.Service,
			Operation: s.Operation,
			SpanID:    s.ID,
			IsError:   s.IsError,
		})
	}

	// Affected services from impact analysis
	impact := g.ImpactAnalysis(triggerService, 3)
	var affected []string
	for _, a := range impact.AffectedServices {
		affected = append(affected, a.Service)
	}

	causalJSON, _ := json.Marshal(causal)
	traceJSON, _ := json.Marshal(traceIDs)
	logsJSON, _ := json.Marshal(firstChain.CorrelatedLogs)
	affectedJSON, _ := json.Marshal(affected)
	spanJSON, _ := json.Marshal(firstChain.SpanChain)

	var metricsJSON []byte
	if len(firstChain.AnomalousMetrics) > 0 {
		metricsJSON, _ = json.Marshal(firstChain.AnomalousMetrics)
	} else {
		metricsJSON = []byte("[]")
	}

	inv := Investigation{
		ID:               id,
		CreatedAt:        time.Now(),
		Status:           "detected",
		Severity:         severity,
		TriggerService:   triggerService,
		TriggerOperation: firstChain.RootCause.Operation,
		ErrorMessage:     firstChain.RootCause.ErrorMessage,
		RootService:      firstChain.RootCause.Service,
		RootOperation:    firstChain.RootCause.Operation,
		CausalChain:      causalJSON,
		TraceIDs:         traceJSON,
		ErrorLogs:        logsJSON,
		AnomalousMetrics: metricsJSON,
		AffectedServices: affectedJSON,
		SpanChain:        spanJSON,
	}

	if err := g.repo.DB().Create(&inv).Error; err != nil {
		slog.Error("Failed to persist investigation", "error", err)
		return
	}

	slog.Info("Investigation persisted", "id", id, "service", triggerService, "severity", severity)
}

// GetInvestigations queries persisted investigations.
func (g *GraphRAG) GetInvestigations(service, severity, status string, limit int) ([]Investigation, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	db := g.repo.DB().Model(&Investigation{}).Order("created_at DESC").Limit(limit)
	if service != "" {
		db = db.Where("trigger_service = ? OR root_service = ?", service, service)
	}
	if severity != "" {
		db = db.Where("severity = ?", severity)
	}
	if status != "" {
		db = db.Where("status = ?", status)
	}

	var investigations []Investigation
	if err := db.Find(&investigations).Error; err != nil {
		return nil, err
	}
	return investigations, nil
}

// GetInvestigation retrieves a single investigation by ID.
func (g *GraphRAG) GetInvestigation(id string) (*Investigation, error) {
	var inv Investigation
	if err := g.repo.DB().Where("id = ?", id).First(&inv).Error; err != nil {
		return nil, err
	}
	return &inv, nil
}

