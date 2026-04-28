package ai

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RandomCodeSpace/otelcontext/internal/storage"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/openai"
)

type Service struct {
	repo       *storage.Repository
	llm        llms.Model
	enabled    bool
	workQueue  chan storage.Log
	workerPool int
	wg         sync.WaitGroup

	// parentCtx is the application-level context. Workers derive their
	// per-call timeout from this so an in-flight LLM call is cancelled
	// when the application is shutting down — rather than blocking
	// shutdown for up to 30s on each worker. Defaults to context.Background
	// when SetParentContext isn't called (preserves legacy behaviour for
	// embedded callers).
	parentCtx context.Context
}

func NewService(repo *storage.Repository) *Service {
	enabled := os.Getenv("AI_ENABLED") == "true"
	if !enabled {
		return &Service{enabled: false}
	}

	// Initialize Azure OpenAI
	opts := []openai.Option{
		openai.WithAPIType(openai.APITypeAzure),
		openai.WithBaseURL(os.Getenv("AZURE_OPENAI_ENDPOINT")),
		openai.WithToken(os.Getenv("AZURE_OPENAI_KEY")),
		openai.WithModel(os.Getenv("AZURE_OPENAI_MODEL")),
	}

	if deployment := os.Getenv("AZURE_OPENAI_DEPLOYMENT"); deployment != "" {
		opts = append(opts, openai.WithModel(deployment))
	}

	if apiVersion := os.Getenv("AZURE_OPENAI_API_VERSION"); apiVersion != "" {
		opts = append(opts, openai.WithAPIVersion(apiVersion))
	}

	llm, err := openai.New(opts...)
	if err != nil {
		log.Printf("Failed to initialize AI service: %v. AI features disabled.", err)
		return &Service{enabled: false}
	}

	queueSize := 100
	if qs := os.Getenv("AI_QUEUE_SIZE"); qs != "" {
		if v, err := strconv.Atoi(qs); err == nil && v > 0 {
			queueSize = v
		}
	}

	workerPool := 3
	if wp := os.Getenv("AI_WORKER_POOL"); wp != "" {
		if v, err := strconv.Atoi(wp); err == nil && v > 0 {
			workerPool = v
		}
	}

	s := &Service{
		repo:       repo,
		llm:        llm,
		enabled:    true,
		workQueue:  make(chan storage.Log, queueSize),
		workerPool: workerPool,
	}

	s.startWorkers()
	return s
}

// SetParentContext wires the application-level context so worker LLM calls
// inherit cancellation on shutdown. Call once during boot before
// EnqueueLog starts taking traffic — the parentCtx is read on every
// dequeued log without locking.
func (s *Service) SetParentContext(ctx context.Context) {
	s.parentCtx = ctx
}

func (s *Service) startWorkers() {
	for i := 0; i < s.workerPool; i++ {
		s.wg.Add(1)
		go func(workerID int) {
			defer s.wg.Done()
			for logEntry := range s.workQueue {
				ctx := s.parentCtx
				if ctx == nil {
					ctx = context.Background()
				}
				s.analyzeLog(ctx, logEntry)
			}
		}(i)
	}
}

func (s *Service) Stop() {
	if !s.enabled {
		return
	}
	close(s.workQueue)
	s.wg.Wait()
}

func (s *Service) EnqueueLog(l storage.Log) {
	if !s.enabled {
		return
	}
	severity := strings.ToUpper(l.Severity)
	if strings.Contains(severity, "ERROR") || strings.Contains(severity, "CRITICAL") || strings.Contains(severity, "FATAL") {
		select {
		case s.workQueue <- l:
		default:
			log.Println("AI work queue full, dropping log analysis")
		}
	}
}

func (s *Service) analyzeLog(ctx context.Context, l storage.Log) {
	prompt := fmt.Sprintf(`Analyze the following error log and provide a brief, actionable insight (max 2 sentences).
	
	Service: %s
	Timestamp: %s
	Severity: %s
	Body: %s
	Attributes: %s
	
	Insight:`, l.ServiceName, l.Timestamp, l.Severity, l.Body, l.AttributesJSON)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	completion, err := llms.GenerateFromSinglePrompt(ctx, s.llm, prompt)
	if err != nil {
		log.Printf("AI Analysis failed for log %d: %v", l.ID, err)
		return
	}

	insight := strings.TrimSpace(completion)
	if insight == "" {
		return
	}

	// UpdateLogInsight is tenant-scoped. The AI worker pool runs under
	// context.Background(), so we reconstitute the tenant from the log itself
	// before calling into the repository.
	tenantCtx := storage.WithTenantContext(ctx, l.TenantID)
	if err := s.repo.UpdateLogInsight(tenantCtx, l.ID, insight); err != nil {
		log.Printf("Failed to save AI insight for log %d: %v", l.ID, err)
	}
}
