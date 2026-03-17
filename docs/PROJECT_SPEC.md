# Project OtelContext - AI Project Specification

**Version:** 5.0  
**Last Updated:** 2025  
**Architecture:** Single-Service Observability Platform

---

## 🎯 Project Overview

### What is Project OtelContext?

Project OtelContext is an integrated observability and AI analysis platform designed for self-hosted deployment in private network environments. It combines distributed tracing, log aggregation, real-time streaming, and AI-powered log analysis into a single, unified binary.

**Core Purpose:**
- Ingest OpenTelemetry Protocol (OTLP) traces and logs from distributed systems
- Provide real-time observability dashboards with service topology visualization
- Stream live telemetry data to connected clients via WebSocket
- Analyze error logs using AI to provide actionable insights
- Operate efficiently in private network environments without external dependencies

**Key Characteristics:**
- **Single Binary Deployment**: Frontend and backend compiled into one executable
- **Self-Hosted First**: Optimized for private infrastructure with no cloud dependencies
- **Real-Time Streaming**: WebSocket-based live data updates with intelligent buffering
- **AI-Powered Analysis**: Automated error log analysis using Azure OpenAI
- **Multi-Database Support**: SQLite, MySQL, PostgreSQL, SQL Server

---

## 🚨 STRICT AI DIRECTIVES (NON-NEGOTIABLE)


### ⛔ Hard Constraints - NEVER VIOLATE

These are absolute rules that must be followed by any AI tool working on this codebase:

1. **NO Express.js**
   - Backend uses native Go `net/http` standard library
   - Do NOT suggest or implement Express.js, Gin, Echo, or any third-party HTTP framework
   - All HTTP routing is done via `http.ServeMux`

2. **NO Tailwind CSS**
   - Frontend uses **Mantine UI v8** component library exclusively
   - Styling is done through Mantine's theming system and component props
   - Do NOT add Tailwind CSS, Bootstrap, or any other CSS framework
   - Custom styles use Mantine's `sx` prop or CSS modules if absolutely necessary

3. **Single-Service Architecture**
   - Frontend and backend are ONE unified binary
   - Frontend is embedded via `go:embed` directive in `web/embed.go`
   - Do NOT split into microservices or separate deployments
   - Do NOT create separate API servers or frontend servers

4. **No Manual Tenant Filters**
   - Entity management should auto-add tenant filter columns as extra fields
   - Do NOT manually define tenant filters in every entity query
   - Use GORM hooks or middleware for automatic tenant scoping

5. **Self-Hosted Infrastructure Priority**
   - Prioritize open-source, self-hosted solutions
   - Avoid cloud-only services (AWS-specific, GCP-specific, etc.)
   - Design for private network deployment
   - External dependencies must be optional and configurable

6. **Private Network Environment**
   - System operates on private networks
   - Relies on internal GitLab Enterprise documentation
   - Do NOT assume public internet access
   - Do NOT use public APIs without explicit configuration

---

## 🏗️ Architecture Overview


### System Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        OtelContext V5.0                               │
│                    (Single Binary)                              │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌──────────────┐         ┌──────────────┐                    │
│  │ gRPC Server  │         │ HTTP Server  │                    │
│  │  Port 4317   │         │  Port 8080   │                    │
│  └──────┬───────┘         └──────┬───────┘                    │
│         │                        │                             │
│         │ OTLP Ingestion         │ REST API + WebSocket        │
│         │                        │                             │
│  ┌──────▼────────────────────────▼───────┐                    │
│  │         Application Core               │                    │
│  │  ┌──────────────────────────────────┐ │                    │
│  │  │  Storage Repository (GORM)       │ │                    │
│  │  │  - Traces, Spans, Logs           │ │                    │
│  │  │  - Multi-DB: SQLite/MySQL/PG/MS  │ │                    │
│  │  └──────────────────────────────────┘ │                    │
│  │                                        │                    │
│  │  ┌──────────────┐  ┌───────────────┐ │                    │
│  │  │ WebSocket    │  │ Event Hub     │ │                    │
│  │  │ Hub          │  │ (Live Mode)   │ │                    │
│  │  │ (Log Stream) │  │ (Snapshots)   │ │                    │
│  │  └──────────────┘  └───────────────┘ │                    │
│  │                                        │                    │
│  │  ┌──────────────┐  ┌───────────────┐ │                    │
│  │  │ AI Service   │  │ Dead Letter   │ │                    │
│  │  │ (3 Workers)  │  │ Queue (DLQ)   │ │                    │
│  │  └──────────────┘  └───────────────┘ │                    │
│  └────────────────────────────────────────┘                    │
│                                                                 │
│  ┌─────────────────────────────────────────┐                  │
│  │  Embedded React Frontend (go:embed)     │                  │
│  │  - Mantine UI Components                │                  │
│  │  - TanStack Query for Data Fetching     │                  │
│  │  - React Context for Live Mode          │                  │
│  └─────────────────────────────────────────┘                  │
└─────────────────────────────────────────────────────────────────┘
```

### Data Flow


#### 1. OTLP Ingestion Flow

```
OpenTelemetry SDK → gRPC (Port 4317) → OTLP Receiver
                                            ↓
                                    Parse & Transform
                                            ↓
                                    ┌───────┴────────┐
                                    │                │
                                Traces            Logs
                                    │                │
                                    ↓                ↓
                            Batch Insert     Batch Insert
                                    │                │
                                    ↓                ↓
                                Database         Database
                                    │                │
                                    └───────┬────────┘
                                            ↓
                                    Success/Failure
                                            │
                                    ┌───────┴────────┐
                                    │                │
                                Success          Failure
                                    │                │
                                    ↓                ↓
                            Broadcast Log    Write to DLQ
                            Enqueue AI       (Retry Later)
                            Notify Events
```

#### 2. Real-Time Streaming Flow

```
Database Insert → Log Callback → ┌─ WebSocket Hub (Buffered)
                                  │   - Buffer: 100 logs
                                  │   - Flush: 500ms or buffer full
                                  │   → Broadcast to all WS clients
                                  │
                                  ├─ AI Service Queue
                                  │   - Filter: ERROR/CRITICAL logs
                                  │   - Workers: 3 concurrent
                                  │   - Queue Size: 100
                                  │   → Azure OpenAI Analysis
                                  │   → Update Log.AIInsight
                                  │
                                  └─ Event Hub (Live Mode)
                                      - Debounce: 5 seconds
                                      - Per-client service filtering
                                      → Compute snapshot (15min window)
                                      → Push to filtered clients
```

#### 3. Frontend Data Flow


```
User Action → React Component → TanStack Query
                                      ↓
                              ┌───────┴────────┐
                              │                │
                          Live Mode        Historical
                              │                │
                              ↓                ↓
                    WebSocket (/ws/events)  REST API
                              │                │
                              ↓                ↓
                    React Query Cache    React Query Cache
                              │                │
                              └───────┬────────┘
                                      ↓
                              Component Re-render
```

**Live Mode Behavior:**
- When `isLive=true`, components use query key `['live', ...]`
- LiveModeContext maintains WebSocket connection to `/ws/events`
- Backend pushes `LiveSnapshot` every 5 seconds (debounced)
- Snapshot contains: Dashboard stats, Traffic, Traces, Service Map
- Data is written directly to React Query cache
- Service filter can be changed dynamically via WebSocket message

**Historical Mode Behavior:**
- When `isLive=false`, components use standard query keys
- TanStack Query fetches from REST API endpoints
- Time range and filters are passed as query parameters
- Standard polling/refetch behavior applies

---

## 💻 Technology Stack

### Backend (Go 1.24.4)

**Core Framework:**
- `net/http` - Native HTTP server (NO third-party frameworks)
- `google.golang.org/grpc` - gRPC server for OTLP ingestion

**Database & ORM:**
- `gorm.io/gorm` v1.31.1 - ORM with repository pattern
- `gorm.io/driver/mysql` - MySQL support
- `gorm.io/driver/postgres` - PostgreSQL support
- `gorm.io/driver/sqlserver` - SQL Server support
- `github.com/glebarez/sqlite` - SQLite (default, embedded)

**Real-Time Communication:**
- `github.com/coder/websocket` v1.8.14 - WebSocket implementation


**AI Integration:**
- `github.com/tmc/langchaingo` v0.1.14 - LangChain Go SDK
- Azure OpenAI integration (configurable endpoint)

**Observability:**
- `github.com/prometheus/client_golang` v1.23.2 - Prometheus metrics
- `log/slog` - Structured logging (Go standard library)

**OpenTelemetry:**
- `go.opentelemetry.io/proto/otlp` v1.9.0 - OTLP protocol definitions
- `go.opentelemetry.io/otel` v1.40.0 - OTel SDK

**Configuration:**
- `github.com/joho/godotenv` v1.5.1 - .env file loading

### Frontend (React 19.2.0 + TypeScript)

**Core Framework:**
- React 19.2.0 with TypeScript
- Vite 7.3.1 - Build tool and dev server

**UI Framework:**
- `@mantine/core` v8.3.15 - Component library (PRIMARY UI FRAMEWORK)
- `@mantine/hooks` v8.3.15 - React hooks utilities
- `@mantine/notifications` v8.3.15 - Toast notifications
- `lucide-react` v0.566.0 - Icon library

**Data Fetching & State:**
- `@tanstack/react-query` v5.90.21 - Server state management
- `@tanstack/react-table` v8.21.3 - Table component
- `@tanstack/react-virtual` v3.13.18 - Virtual scrolling
- React Context API - Global state (LiveModeContext)

**Visualization:**
- `echarts` v6.0.0 - Charting library
- `echarts-for-react` v3.0.6 - React wrapper for ECharts
- `reactflow` v11.11.4 - Service topology graph
- `dagre` v0.8.5 - Graph layout algorithm

**Animation:**
- `framer-motion` v12.34.1 - Animation library

**Utilities:**
- `date-fns` v4.1.0 - Date manipulation

---

## 📁 Project Structure


```
OtelContext/
├── main.go                      # Application entry point
├── go.mod                       # Go module dependencies
├── go.sum                       # Dependency checksums
├── .env.example                 # Environment configuration template
├── README.md                    # Project documentation
│
├── internal/                    # Private application code
│   ├── ai/
│   │   └── service.go          # AI log analysis service (Azure OpenAI)
│   │
│   ├── api/
│   │   ├── handlers.go         # HTTP API handlers
│   │   └── handlers_v2.go      # Extended API handlers
│   │
│   ├── config/
│   │   └── config.go           # Configuration loading (.env)
│   │
│   ├── ingest/
│   │   └── otlp.go             # OTLP gRPC receivers (traces + logs)
│   │
│   ├── middleware/
│   │   └── (empty)             # Reserved for HTTP middleware
│   │
│   ├── queue/
│   │   └── dlq.go              # Dead Letter Queue implementation
│   │
│   ├── realtime/
│   │   ├── hub.go              # Buffered WebSocket hub (log streaming)
│   │   └── events_ws.go        # Event notification hub (live mode)
│   │
│   ├── storage/
│   │   ├── models.go           # GORM data models (Trace, Span, Log)
│   │   ├── repository.go       # Basic repository methods
│   │   ├── repository_v2.go    # Advanced queries (dashboard, metrics)
│   │   └── factory.go          # Database connection factory
│   │
│   └── telemetry/
│       ├── metrics.go          # Prometheus metrics
│       └── health_ws.go        # Health check WebSocket
│
├── web/                         # Frontend application
│   ├── embed.go                # Go embed directive for dist/
│   ├── package.json            # NPM dependencies
│   ├── vite.config.ts          # Vite build configuration
│   ├── tsconfig.json           # TypeScript configuration
│   │
│   └── src/
│       ├── main.tsx            # React entry point
│       ├── App.tsx             # Root component
│       ├── theme.ts            # Mantine theme configuration
│       ├── types.ts            # TypeScript type definitions
│       │
│       ├── api/                # API client functions
│       │
│       ├── contexts/
│       │   └── LiveModeContext.tsx  # Global live mode state
│       │
│       ├── hooks/
│       │   └── useFilterParams.ts   # URL query param hooks
│       │
│       ├── layouts/
│       │   └── AppLayout.tsx   # Main application layout
│       │
│       ├── components/
│       │   └── TimeRangeSelector.tsx  # Shared components
│       │
│       └── features/           # Feature-based organization
│           ├── dashboard/
│           │   └── Dashboard.tsx
│           ├── logs/
│           │   └── LogExplorer.tsx
│           ├── traces/
│           │   └── TraceExplorer.tsx
│           ├── topology/
│           │   └── ServiceMap.tsx
│           └── settings/
│               └── Settings.tsx
│
├── data/                        # Runtime data directory
│   └── dlq/                    # Dead Letter Queue files
│
└── test/                        # Test services and scripts
    ├── authservice/
    ├── orderservice/
    └── ...                     # Microservice simulators for testing
```

---

## 🗄️ Database Schema


### Core Models

#### Trace
Represents a complete distributed trace (root span + metadata).

```go
type Trace struct {
    ID          uint           // Primary key
    TraceID     string         // Unique trace identifier (32 chars, indexed)
    ServiceName string         // Originating service (indexed)
    Duration    int64          // Total duration in microseconds (indexed)
    Status      string         // OK, ERROR, etc.
    Timestamp   time.Time      // Trace start time (indexed)
    Spans       []Span         // Related spans (foreign key)
    Logs        []Log          // Related logs (foreign key)
    CreatedAt   time.Time
    UpdatedAt   time.Time
    DeletedAt   gorm.DeletedAt // Soft delete support
}
```

**Indexes:**
- `trace_id` (unique)
- `service_name`
- `duration`
- `timestamp`
- `deleted_at`

#### Span
Represents a single operation within a trace.

```go
type Span struct {
    ID             uint
    TraceID        string    // Links to Trace (indexed)
    SpanID         string    // Unique span identifier (16 chars)
    ParentSpanID   string    // Parent span ID (for hierarchy)
    OperationName  string    // Operation/method name (indexed)
    StartTime      time.Time
    EndTime        time.Time
    Duration       int64     // Duration in microseconds
    ServiceName    string    // Service that created this span (indexed)
    AttributesJSON string    // JSON-encoded attributes (text field)
}
```

**Indexes:**
- `trace_id`
- `operation_name`
- `service_name`

#### Log
Represents a log entry, optionally linked to a trace/span.

```go
type Log struct {
    ID             uint
    TraceID        string    // Optional trace association (indexed)
    SpanID         string    // Optional span association
    Severity       string    // INFO, WARN, ERROR, etc. (indexed)
    Body           string    // Log message (text field)
    ServiceName    string    // Service that emitted log (indexed)
    AttributesJSON string    // JSON-encoded attributes (text field)
    AIInsight      string    // AI-generated insight (text field)
    Timestamp      time.Time // Log timestamp (indexed)
}
```

**Indexes:**
- `trace_id`
- `severity`
- `service_name`
- `timestamp`

### Database Support

**Supported Drivers:**
- SQLite (default) - Embedded, zero-config
- MySQL - Production-ready, high performance
- PostgreSQL - Advanced features, JSON support
- SQL Server - Enterprise environments

**Configuration:**
```bash
# SQLite (default)
DB_DRIVER=sqlite
DB_DSN=OtelContext.db

# MySQL
DB_DRIVER=mysql
DB_DSN=user:password@tcp(host:3306)/OtelContext?charset=utf8mb4&parseTime=True&loc=Local

# PostgreSQL
DB_DRIVER=postgres
DB_DSN=host=localhost user=OtelContext password=secret dbname=OtelContext port=5432 sslmode=disable

# SQL Server
DB_DRIVER=sqlserver
DB_DSN=sqlserver://user:password@host:1433?database=OtelContext
```

---

## 🔌 API Endpoints


### REST API (Port 8080)

#### Traces
- `GET /api/traces` - List traces with filtering and pagination
  - Query params: `start`, `end`, `service_name[]`, `status`, `search`, `limit`, `offset`, `sort_by`, `order_by`
  - Returns: `TracesResponse` with pagination metadata

#### Logs
- `GET /api/logs` - List logs with filtering
  - Query params: `service_name`, `severity`, `search`, `start`, `end`, `limit`, `offset`
  - Returns: Array of logs with total count

- `GET /api/logs/context` - Get logs surrounding a timestamp
  - Query params: `timestamp`
  - Returns: Logs within ±1 minute window

- `GET /api/logs/{id}/insight` - Get AI insight for a specific log
  - Returns: `{"insight": "..."}`

#### Metrics
- `GET /api/metrics/dashboard` - Dashboard statistics
  - Query params: `start`, `end`, `service_name[]`
  - Returns: `DashboardStats` (total traces, errors, latency, etc.)

- `GET /api/metrics/traffic` - Traffic over time
  - Query params: `start`, `end`, `service_name[]`
  - Returns: Array of `TrafficPoint` (timestamp, count, error_count)

- `GET /api/metrics/latency_heatmap` - Latency distribution
  - Query params: `start`, `end`, `service_name[]`
  - Returns: Array of `LatencyPoint` (timestamp, duration)

- `GET /api/metrics/service-map` - Service topology with metrics
  - Query params: `start`, `end`
  - Returns: `ServiceMapMetrics` (nodes, edges with call counts)

#### Metadata
- `GET /api/metadata/services` - List all service names
  - Returns: Array of strings

#### Health & Monitoring
- `GET /api/health` - Health check with telemetry
  - Returns: `HealthStats` (ingestion rate, DLQ size, active connections)

- `GET /metrics` - Prometheus metrics endpoint
  - Returns: Prometheus text format

#### Admin
- `DELETE /api/admin/purge` - Purge old data
  - Query params: `days` (default: 7)
  - Returns: Count of purged logs and traces

- `POST /api/admin/vacuum` - Vacuum database (SQLite only)
  - Returns: `{"status": "vacuumed"}`

### WebSocket Endpoints

#### Log Streaming
- `WS /ws` - Real-time log streaming
  - Protocol: Buffered broadcast
  - Buffer: 100 logs or 500ms flush interval
  - Format: JSON array of `LogEntry` objects
  - Behavior: Broadcasts ALL logs to all clients

#### Live Mode Events
- `WS /ws/events` - Live mode data snapshots
  - Protocol: Server push with client filtering
  - Flush: Every 5 seconds (debounced)
  - Format: `LiveSnapshot` JSON object
  - Client can send: `{"service": "service-name"}` to filter
  - Returns: Dashboard, Traffic, Traces, ServiceMap for last 15 minutes

#### Health Monitoring
- `WS /ws/health` - Real-time health metrics
  - Protocol: Server push
  - Format: `HealthStats` JSON object

### gRPC Endpoints (Port 4317)

#### OTLP Receivers
- `TraceService.Export` - Ingest OTLP traces
  - Protocol: `opentelemetry.proto.collector.trace.v1.TraceService`
  - Compression: gzip supported

- `LogsService.Export` - Ingest OTLP logs
  - Protocol: `opentelemetry.proto.collector.logs.v1.LogsService`
  - Compression: gzip supported

---

## 🎨 Frontend Architecture


### Component Organization

**Feature-Based Structure:**
- Each feature has its own directory under `web/src/features/`
- Features are self-contained with components, hooks, and logic
- Shared components live in `web/src/components/`

**Key Features:**
1. **Dashboard** - Overview metrics, traffic charts, error rates
2. **Traces** - Trace explorer with filtering and detail view
3. **Logs** - Log explorer with search and AI insights
4. **Topology** - Service map visualization with ReactFlow
5. **Settings** - Configuration and admin tools

### State Management

**TanStack React Query:**
- Primary state management for server data
- Automatic caching, refetching, and background updates
- Query keys follow pattern: `['resource', ...filters]`
- Live mode uses special keys: `['live', 'resource']`

**React Context:**
- `LiveModeContext` - Global live mode state
  - Manages WebSocket connection to `/ws/events`
  - Writes snapshots directly to React Query cache
  - Handles service filtering
  - Auto-reconnects on disconnect

**URL State:**
- Time ranges, filters, and view state stored in URL query params
- Custom hook: `useFilterParams` for type-safe URL state
- Enables shareable links and browser back/forward

### Styling Approach

**Mantine UI System:**
- All components use Mantine's component library
- Theme configured in `web/src/theme.ts`
- Primary color: Indigo
- Font: Inter
- Default radius: Medium

**Styling Methods (in order of preference):**
1. Mantine component props (`color`, `size`, `variant`, etc.)
2. Mantine `sx` prop for custom styles
3. CSS modules (only if absolutely necessary)

**DO NOT USE:**
- Tailwind CSS classes
- Inline styles (except for dynamic values)
- Global CSS (except for resets)

### Data Fetching Patterns

#### Historical Mode (isLive = false)
```typescript
const { data } = useQuery({
  queryKey: ['traces', filters],
  queryFn: () => fetchTraces(filters),
  refetchInterval: 30000, // Optional polling
})
```

#### Live Mode (isLive = true)
```typescript
const { data } = useQuery({
  queryKey: ['live', 'traces'],
  queryFn: () => {
    // Initial fetch or fallback
    return fetchTraces(liveFilters)
  },
  // Data is updated by LiveModeContext via WebSocket
})
```

**Live Mode Behavior:**
- WebSocket connection managed by `LiveModeContext`
- Backend pushes `LiveSnapshot` every 5 seconds
- Context writes data to React Query cache using `queryClient.setQueryData`
- Components automatically re-render when cache updates
- Service filter sent over WebSocket: `ws.send(JSON.stringify({ service: 'name' }))`

---

## 🔄 Key Design Patterns


### Backend Patterns

#### 1. Repository Pattern
All database access goes through the `Repository` struct in `internal/storage/`.

**Benefits:**
- Centralized data access logic
- Easy to mock for testing
- Database-agnostic queries
- Consistent error handling

**Example:**
```go
type Repository struct {
    db      *gorm.DB
    driver  string
    metrics *telemetry.Metrics
}

func (r *Repository) GetTraces(limit, offset int) ([]Trace, error) {
    var traces []Trace
    err := r.db.Order("timestamp desc").Limit(limit).Offset(offset).Find(&traces).Error
    return traces, err
}
```

#### 2. Buffered WebSocket Hub
The `realtime.Hub` buffers logs before broadcasting to prevent UI freezing at high throughput.

**Configuration:**
- Buffer size: 100 logs
- Flush interval: 500ms
- Flush triggers: Buffer full OR timer fires

**Benefits:**
- Reduces WebSocket message frequency
- Prevents client overwhelm
- Maintains real-time feel
- Automatic slow client disconnection

**Implementation:**
```go
type Hub struct {
    buffer        []LogEntry
    maxBufferSize int           // 100
    flushInterval time.Duration // 500ms
}

// Flush sends buffered logs as JSON array
func (h *Hub) flush() {
    batch := h.buffer
    h.buffer = make([]LogEntry, 0, h.maxBufferSize)
    data, _ := json.Marshal(batch)
    
    for client := range h.clients {
        select {
        case client.send <- data:
        default:
            // Disconnect slow clients
            delete(h.clients, client)
        }
    }
}
```

#### 3. Event Hub with Per-Client Filtering
The `realtime.EventHub` pushes data snapshots filtered by service name.

**Features:**
- Debounced updates (5 second flush)
- Per-client service filtering
- 15-minute rolling window
- Snapshot includes: Dashboard, Traffic, Traces, ServiceMap

**Client Filter Update:**
```go
// Client sends: {"service": "auth-service"}
// Hub updates filter and sends filtered snapshot
```

#### 4. AI Worker Pool
The `ai.Service` uses a worker pool pattern for concurrent log analysis.

**Configuration:**
- Workers: 3 concurrent
- Queue size: 100 logs
- Timeout: 30 seconds per analysis
- Filter: ERROR, CRITICAL, FATAL severity only

**Flow:**
```
Log Ingestion → Severity Check → Enqueue → Worker Pool → Azure OpenAI → Update DB
```

**Backpressure Handling:**
- If queue is full, drop log analysis (don't block ingestion)
- Logs are still stored, just not analyzed immediately

#### 5. Dead Letter Queue (DLQ)
Disk-based resilience for failed database writes.

**Behavior:**
- On batch insert failure, serialize to JSON and write to disk
- Background worker replays files every 5 minutes (configurable)
- On successful replay, delete file
- Prometheus metric tracks DLQ size

**Directory Structure:**
```
data/dlq/
├── batch_1234567890.json
├── batch_1234567891.json
└── ...
```

#### 6. Callback Pattern for Real-Time Updates
OTLP receivers use callbacks to trigger downstream actions without tight coupling.

```go
type LogsServer struct {
    logCallback func(storage.Log)
}

// In main.go
logsServer.SetLogCallback(func(l storage.Log) {
    apiServer.BroadcastLog(l)      // WebSocket
    aiService.EnqueueLog(l)         // AI analysis
    eventHub.NotifyRefresh()        // Live mode
})
```

---

## ⚙️ Configuration


### Environment Variables

#### Application
```bash
APP_ENV=development              # Environment: development, production
LOG_LEVEL=INFO                   # Logging level: DEBUG, INFO, WARN, ERROR
HTTP_PORT=8080                   # HTTP server port
GRPC_PORT=4317                   # gRPC OTLP receiver port
```

#### Database
```bash
DB_DRIVER=sqlite                 # Database driver: sqlite, mysql, postgres, sqlserver
DB_DSN=OtelContext.db                  # Database connection string (driver-specific)
```

#### Dead Letter Queue
```bash
DLQ_PATH=./data/dlq              # DLQ directory path
DLQ_REPLAY_INTERVAL=5m           # Replay interval (Go duration format)
```

#### Ingestion Filtering
```bash
INGEST_MIN_SEVERITY=INFO         # Minimum log severity to ingest
INGEST_ALLOWED_SERVICES=         # Comma-separated list of allowed services (empty = all)
INGEST_EXCLUDED_SERVICES=        # Comma-separated list of excluded services
```

#### AI Service (Optional)
```bash
AI_ENABLED=true                  # Enable AI log analysis
AZURE_OPENAI_ENDPOINT=           # Azure OpenAI endpoint URL
AZURE_OPENAI_KEY=                # Azure OpenAI API key
AZURE_OPENAI_MODEL=              # Model name (e.g., gpt-4)
AZURE_OPENAI_DEPLOYMENT=         # Deployment name (Azure-specific)
AZURE_OPENAI_API_VERSION=        # API version (e.g., 2023-05-15)
```

### Configuration Loading

**Priority Order:**
1. Environment variables (highest priority)
2. `.env` file in working directory
3. Default values (lowest priority)

**Implementation:**
```go
func Load() *Config {
    godotenv.Load(".env") // Ignore errors, use env vars or defaults
    return &Config{
        HTTPPort: getEnv("HTTP_PORT", "8080"),
        // ...
    }
}
```

---

## 🚀 Build & Deployment

### Development Mode

**Backend:**
```bash
# Install Air for hot reload (optional)
go install github.com/air-verse/air@latest

# Run with Air
air

# Or run directly
go run main.go
```

**Frontend:**
```bash
cd web
npm install
npm run dev  # Vite dev server on port 5173
```

**Full Stack Development:**
- Backend runs on port 8080
- Frontend dev server on port 5173 with proxy to backend
- Hot reload for both frontend and backend

### Production Build

**Build Frontend:**
```bash
cd web
npm run build  # Outputs to web/dist/
```

**Build Binary:**
```bash
go build -o OtelContext main.go
```

**Result:**
- Single binary: `OtelContext` (or `OtelContext.exe` on Windows)
- Frontend embedded via `go:embed` in `web/embed.go`
- No external dependencies except database

### Deployment

**Single Binary Deployment:**
```bash
# Copy binary to server
scp OtelContext user@server:/opt/OtelContext/

# Create .env file
cat > /opt/OtelContext/.env << EOF
DB_DRIVER=mysql
DB_DSN=user:password@tcp(localhost:3306)/OtelContext
EOF

# Run
cd /opt/OtelContext
./OtelContext
```

**Docker Deployment (Future):**
```dockerfile
FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY . .
RUN cd web && npm install && npm run build
RUN go build -o OtelContext main.go

FROM alpine:latest
COPY --from=builder /app/OtelContext /OtelContext
EXPOSE 8080 4317
CMD ["/OtelContext"]
```

### Database Migration

**Automatic Migration:**
- GORM AutoMigrate runs on startup
- Creates tables if they don't exist
- Adds new columns for schema changes
- Does NOT drop columns or tables

**Manual Migration:**
- For complex schema changes, use GORM Migrator API
- Or use external migration tools (golang-migrate, etc.)

---

## 📊 Monitoring & Observability


### Internal Telemetry

OtelContext monitors itself using Prometheus metrics.

**Metrics Exposed:**

1. **OtelContext_ingestion_rate** (Counter)
   - Total spans and logs ingested
   - Incremented on successful batch insert

2. **OtelContext_active_connections** (Gauge)
   - Number of active WebSocket clients
   - Updated on connect/disconnect

3. **OtelContext_db_latency** (Histogram)
   - Database operation latency in seconds
   - Measured via GORM callbacks

4. **OtelContext_dlq_size** (Gauge)
   - Number of files in Dead Letter Queue
   - Updated every 30 seconds

**Prometheus Endpoint:**
```
GET /metrics
```

**Health Check Endpoint:**
```
GET /api/health

Response:
{
  "ingestion_rate": 12345,
  "dlq_size": 0,
  "active_connections": 5,
  "db_latency_p99_ms": 12.5
}
```

### Structured Logging

**Logger:** Go standard library `log/slog`

**Log Levels:**
- DEBUG - Detailed diagnostic information
- INFO - General informational messages (default)
- WARN - Warning messages
- ERROR - Error messages

**Configuration:**
```bash
LOG_LEVEL=DEBUG  # Set via environment variable
```

**Log Format:**
```
time=2025-01-15T10:30:45.123Z level=INFO msg="🚀 Starting OtelContext V5.0" env=development log_level=INFO
```

**Key Log Events:**
- Application startup/shutdown
- Database connections
- WebSocket client connections/disconnections
- OTLP ingestion batches
- DLQ replay operations
- AI analysis completions
- Errors and warnings

---

## 🔒 Security Considerations


### Current Security Posture

**Private Network Deployment:**
- Designed for internal, trusted networks
- No built-in authentication/authorization
- Assumes network-level security (VPN, firewall, etc.)

**Input Validation:**
- SQL injection prevented by GORM parameterized queries
- Query parameter whitelisting for sort fields
- OTLP protocol validation via protobuf

**WebSocket Security:**
- `InsecureSkipVerify: true` for development
- Should be configured for production (CORS, origin checks)

### Security Recommendations

**For Production Deployment:**

1. **Add Authentication:**
   - Implement middleware for API key or JWT validation
   - Protect admin endpoints (`/api/admin/*`)
   - Consider OAuth2/OIDC for enterprise SSO

2. **Enable TLS:**
   - Use reverse proxy (nginx, Caddy) for HTTPS
   - Configure TLS for gRPC OTLP receiver
   - Secure WebSocket connections (wss://)

3. **Rate Limiting:**
   - Implement rate limiting on API endpoints
   - Protect against ingestion floods
   - Limit WebSocket connections per IP

4. **Database Security:**
   - Use least-privilege database users
   - Enable database encryption at rest
   - Secure connection strings (use secrets management)

5. **Network Segmentation:**
   - Isolate OTLP receiver (port 4317) from public access
   - Restrict HTTP API (port 8080) to internal network
   - Use firewall rules to limit access

6. **Audit Logging:**
   - Log all admin operations
   - Track data purge operations
   - Monitor failed authentication attempts

### Sensitive Data Handling

**PII in Logs:**
- OtelContext stores log bodies and attributes as-is
- No automatic PII redaction
- Recommendation: Configure OTLP exporters to redact PII before sending

**AI Analysis:**
- Log bodies sent to Azure OpenAI for analysis
- Ensure Azure OpenAI endpoint is in same region/network
- Consider data residency requirements

**Database Credentials:**
- Store in `.env` file with restricted permissions (chmod 600)
- Use environment variables in production
- Consider secrets management (Vault, AWS Secrets Manager)

---

## 🧪 Testing Approach


### Test Infrastructure

**Test Services:**
The `test/` directory contains microservice simulators for generating test data:
- authservice
- orderservice
- paymentservice
- inventoryservice
- shippingservice
- notificationservice
- userservice

**Test Scripts:**
- `test/run_simulation.ps1` - PowerShell simulation runner
- `test/run_simulation.sh` - Bash simulation runner
- `test/load_test.ps1` - Load testing script

### Testing Strategy

**Backend Testing:**
- Unit tests for repository methods
- Integration tests with in-memory SQLite
- OTLP ingestion tests with mock data

**Frontend Testing:**
- Component tests with React Testing Library
- E2E tests with Playwright (future)
- Visual regression tests (future)

**Load Testing:**
- Simulate high-throughput OTLP ingestion
- Test WebSocket broadcast performance
- Measure database query performance under load

### Manual Testing

**OTLP Integration Test:**
```bash
# Start OtelContext
./OtelContext

# Send test trace via grpcurl
grpcurl -plaintext -d @ localhost:4317 \
  opentelemetry.proto.collector.trace.v1.TraceService/Export < test_trace.json
```

**WebSocket Test:**
```javascript
// Browser console
const ws = new WebSocket('ws://localhost:8080/ws')
ws.onmessage = (e) => console.log(JSON.parse(e.data))
```

---

## 🔧 Development Workflow


### Adding a New Feature

#### Backend (Go)

1. **Add Model (if needed):**
   - Define struct in `internal/storage/models.go`
   - Add GORM tags for indexes and relationships
   - Run application to auto-migrate

2. **Add Repository Methods:**
   - Add query methods to `internal/storage/repository.go` or `repository_v2.go`
   - Use GORM query builder
   - Return errors for proper handling

3. **Add API Handler:**
   - Create handler function in `internal/api/handlers.go`
   - Register route in `RegisterRoutes()`
   - Use pattern: `mux.HandleFunc("GET /api/resource", handler)`

4. **Update Telemetry (if needed):**
   - Add Prometheus metrics in `internal/telemetry/metrics.go`
   - Instrument code with metric calls

#### Frontend (React)

1. **Create Feature Directory:**
   ```
   web/src/features/my-feature/
   ├── MyFeature.tsx        # Main component
   ├── components/          # Feature-specific components
   ├── hooks/               # Feature-specific hooks
   └── types.ts             # Feature-specific types
   ```

2. **Add API Client:**
   - Create fetch function in `web/src/api/`
   - Use TypeScript for type safety

3. **Add React Query Hook:**
   ```typescript
   export function useMyFeature() {
     return useQuery({
       queryKey: ['myFeature'],
       queryFn: fetchMyFeature,
     })
   }
   ```

4. **Build UI with Mantine:**
   - Use Mantine components exclusively
   - Follow existing patterns for layout and styling
   - Add to navigation in `AppLayout.tsx`

### Code Style Guidelines

**Go:**
- Follow standard Go formatting (`gofmt`)
- Use meaningful variable names
- Add comments for exported functions
- Handle errors explicitly (no silent failures)
- Use structured logging with `slog`

**TypeScript/React:**
- Use functional components with hooks
- Prefer `const` over `let`
- Use TypeScript strict mode
- Add JSDoc comments for complex functions
- Use Mantine components, not custom CSS

**Naming Conventions:**
- Go: PascalCase for exported, camelCase for unexported
- TypeScript: PascalCase for components, camelCase for functions/variables
- Files: kebab-case for filenames
- Database: snake_case for column names

---

## 🐛 Common Issues & Solutions


### Backend Issues

**Issue: Database connection fails**
```
Solution:
1. Check DB_DRIVER and DB_DSN in .env
2. Verify database server is running
3. Test connection string manually
4. Check firewall rules
```

**Issue: OTLP ingestion not working**
```
Solution:
1. Verify gRPC port 4317 is accessible
2. Check OTLP exporter configuration (endpoint, protocol)
3. Enable DEBUG logging to see ingestion attempts
4. Test with grpcurl or OTLP test client
```

**Issue: WebSocket clients disconnecting**
```
Solution:
1. Check for slow clients (automatic disconnect)
2. Verify network stability
3. Increase buffer size if high throughput
4. Check browser console for errors
```

**Issue: DLQ files accumulating**
```
Solution:
1. Check database write performance
2. Verify database has sufficient disk space
3. Review DLQ replay logs for errors
4. Manually replay files if needed
```

### Frontend Issues

**Issue: Live mode not updating**
```
Solution:
1. Check WebSocket connection status in DevTools
2. Verify backend is pushing snapshots (check logs)
3. Check React Query DevTools for cache updates
4. Ensure isLive is true in LiveModeContext
```

**Issue: Build fails with TypeScript errors**
```
Solution:
1. Run `npm install` to update dependencies
2. Check for type mismatches in API responses
3. Verify all imports are correct
4. Run `npm run build` to see full error output
```

**Issue: Mantine components not styled correctly**
```
Solution:
1. Verify MantineProvider wraps the app
2. Check theme configuration in theme.ts
3. Ensure postcss.config.cjs is present
4. Clear Vite cache: rm -rf node_modules/.vite
```

---

## 📚 Key Concepts & Terminology


### Observability Terms

**Trace:**
- A complete request flow through a distributed system
- Composed of multiple spans
- Has a unique TraceID (32 characters)

**Span:**
- A single operation within a trace
- Has a SpanID (16 characters) and optional ParentSpanID
- Contains timing, attributes, and status

**Log:**
- A timestamped message from a service
- Can be associated with a trace/span
- Has severity level (INFO, WARN, ERROR, etc.)

**Service:**
- A logical component of the system
- Identified by service.name attribute
- Appears as a node in the service map

**OTLP (OpenTelemetry Protocol):**
- Standard protocol for telemetry data
- Supports traces, logs, and metrics
- Uses gRPC or HTTP transport

### OtelContext-Specific Terms

**Live Mode:**
- Real-time data streaming mode
- Uses WebSocket for push updates
- Shows last 15 minutes of data
- Supports per-client service filtering

**Historical Mode:**
- Query-based data retrieval
- Uses REST API with time range filters
- Supports pagination and sorting

**Buffered Hub:**
- WebSocket broadcast mechanism
- Buffers logs before sending
- Prevents UI freezing at high throughput

**Event Hub:**
- Live mode snapshot broadcaster
- Debounces rapid updates
- Pushes complete data snapshots

**Dead Letter Queue (DLQ):**
- Disk-based failure recovery
- Stores failed database writes
- Automatic replay mechanism

**AI Insight:**
- AI-generated analysis of error logs
- Stored in Log.AIInsight field
- Generated by Azure OpenAI

---

## 🎯 Design Decisions & Rationale


### Why Single Binary?

**Decision:** Embed frontend into Go binary via `go:embed`

**Rationale:**
- Simplified deployment (one file to copy)
- No need for separate web server
- Reduced operational complexity
- Easier version management
- Better for self-hosted environments

**Trade-offs:**
- Larger binary size (~20-30MB)
- Frontend changes require full rebuild
- No CDN benefits for static assets

### Why Native net/http?

**Decision:** Use Go standard library `net/http` instead of frameworks

**Rationale:**
- Zero external dependencies for HTTP
- Excellent performance
- Stable API (part of Go stdlib)
- Sufficient for our routing needs
- Easier to understand and maintain

**Trade-offs:**
- More verbose routing code
- No built-in middleware ecosystem
- Manual parameter parsing

### Why Buffered WebSocket?

**Decision:** Buffer logs before broadcasting (100 logs or 500ms)

**Rationale:**
- Prevents UI freezing at high ingestion rates
- Reduces WebSocket message frequency
- Maintains real-time feel (500ms is imperceptible)
- Automatic backpressure handling

**Trade-offs:**
- Slight delay in log visibility (max 500ms)
- More complex implementation
- Memory overhead for buffer

### Why Two WebSocket Endpoints?

**Decision:** Separate `/ws` (logs) and `/ws/events` (snapshots)

**Rationale:**
- Different use cases: streaming vs. snapshots
- Different update frequencies: real-time vs. 5 seconds
- Different data shapes: individual logs vs. aggregated metrics
- Allows independent scaling and optimization

**Trade-offs:**
- More complex client-side logic
- Two connections per client in live mode

### Why GORM?

**Decision:** Use GORM as ORM instead of raw SQL

**Rationale:**
- Multi-database support (SQLite, MySQL, PostgreSQL, SQL Server)
- Automatic migrations
- Type-safe queries
- Reduced boilerplate
- Active community and maintenance

**Trade-offs:**
- Performance overhead vs. raw SQL
- Learning curve for complex queries
- Potential for N+1 queries if not careful

### Why Mantine UI?

**Decision:** Use Mantine instead of Tailwind or custom CSS

**Rationale:**
- Complete component library (no need to build from scratch)
- Consistent design system
- Built-in dark mode support
- TypeScript-first
- Excellent documentation

**Trade-offs:**
- Larger bundle size vs. Tailwind
- Less flexibility than custom CSS
- Opinionated design patterns

### Why TanStack Query?

**Decision:** Use TanStack Query for server state

**Rationale:**
- Automatic caching and refetching
- Built-in loading and error states
- Optimistic updates support
- DevTools for debugging
- Industry standard for React data fetching

**Trade-offs:**
- Learning curve for advanced features
- Additional dependency
- Requires understanding of cache invalidation

---

## 🔮 Future Enhancements


### Planned Features

**Authentication & Authorization:**
- API key authentication
- Role-based access control (RBAC)
- OAuth2/OIDC integration
- Multi-tenancy support

**Advanced Analytics:**
- Anomaly detection using ML
- Predictive alerting
- Trend analysis
- Custom dashboards

**Alerting:**
- Rule-based alerting
- Webhook notifications
- Email/Slack integration
- Alert history and acknowledgment

**Data Retention:**
- Configurable retention policies
- Automatic data archival
- Cold storage integration
- Data compression

**Performance Optimizations:**
- Query result caching (Redis)
- Database read replicas
- Horizontal scaling support
- Metrics pre-aggregation

**Enhanced AI Features:**
- Root cause analysis
- Automated incident summarization
- Log pattern recognition
- Suggested fixes for common errors

**Metrics Support:**
- OTLP metrics ingestion
- Prometheus-compatible metrics storage
- Custom metric dashboards
- Metric alerting

**Distributed Tracing Enhancements:**
- Trace comparison
- Trace search by attributes
- Span-level annotations
- Critical path analysis

---

## 📖 Additional Resources

### Documentation

**Internal Documentation:**
- `README.md` - Project overview and quick start
- `AI_PROJECT_SPEC.md` - This document (comprehensive reference)
- `.env.example` - Configuration template

**External Documentation:**
- [OpenTelemetry Specification](https://opentelemetry.io/docs/specs/otlp/)
- [GORM Documentation](https://gorm.io/docs/)
- [Mantine UI Documentation](https://mantine.dev/)
- [TanStack Query Documentation](https://tanstack.com/query/latest)

### Related Projects

**OpenTelemetry:**
- [OpenTelemetry Go SDK](https://github.com/open-telemetry/opentelemetry-go)
- [OpenTelemetry Collector](https://github.com/open-telemetry/opentelemetry-collector)

**Similar Tools:**
- Jaeger - Distributed tracing
- Grafana Tempo - Trace storage
- Loki - Log aggregation
- Zipkin - Distributed tracing

---

## 🤝 Contributing Guidelines


### Code Contribution Process

1. **Understand the Architecture:**
   - Read this AI_PROJECT_SPEC.md thoroughly
   - Review existing code patterns
   - Check STRICT AI DIRECTIVES section

2. **Follow Design Patterns:**
   - Use repository pattern for data access
   - Follow feature-based organization for frontend
   - Maintain single-service architecture

3. **Respect Technology Choices:**
   - NO Express.js (use net/http)
   - NO Tailwind CSS (use Mantine UI)
   - NO microservices (keep single binary)

4. **Write Clean Code:**
   - Follow Go and TypeScript best practices
   - Add comments for complex logic
   - Use meaningful variable names
   - Handle errors explicitly

5. **Test Your Changes:**
   - Test with multiple databases (SQLite, MySQL)
   - Verify WebSocket functionality
   - Check live mode behavior
   - Test with high ingestion rates

6. **Document Changes:**
   - Update README.md if needed
   - Update this spec for architectural changes
   - Add inline comments for complex code

### AI Tool Guidelines

**When using AI tools (like this one) on this codebase:**

1. **Always reference this document first**
2. **Never violate STRICT AI DIRECTIVES**
3. **Maintain existing patterns and conventions**
4. **Ask for clarification if uncertain**
5. **Test generated code thoroughly**
6. **Review for security implications**

**AI-Friendly Practices:**
- Clear, descriptive function names
- Comprehensive type definitions
- Inline documentation for complex logic
- Consistent file organization
- Explicit error handling

---

## 📝 Version History


**V5.0 (Current) - Production Hardened Edition**
- Single binary deployment with embedded frontend
- Buffered WebSocket hub for high-throughput streaming
- Event hub with per-client service filtering
- AI-powered log analysis with worker pool
- Dead Letter Queue for resilience
- Multi-database support (SQLite, MySQL, PostgreSQL, SQL Server)
- Prometheus metrics and health checks
- Live mode with 15-minute rolling window
- Service topology visualization

**Previous Versions:**
- V4.x - Enhanced real-time features
- V3.x - AI integration
- V2.x - WebSocket streaming
- V1.x - Basic OTLP ingestion

---

## 🎓 Learning Path for New Developers

### Week 1: Understanding the Basics
1. Read this entire AI_PROJECT_SPEC.md
2. Review main.go to understand startup flow
3. Explore internal/storage/ to understand data models
4. Run the application locally with SQLite

### Week 2: Backend Deep Dive
1. Study OTLP ingestion in internal/ingest/
2. Understand repository pattern in internal/storage/
3. Review WebSocket hubs in internal/realtime/
4. Explore API handlers in internal/api/

### Week 3: Frontend Deep Dive
1. Study LiveModeContext for WebSocket integration
2. Review TanStack Query usage patterns
3. Explore Mantine UI components in features/
4. Understand routing and navigation

### Week 4: Advanced Topics
1. Study AI service worker pool
2. Understand Dead Letter Queue mechanism
3. Review Prometheus metrics implementation
4. Explore service map computation algorithm

---

## 🔍 Quick Reference

### Key Files to Know

**Backend:**
- `main.go` - Application entry point, initialization order
- `internal/storage/models.go` - Data models (Trace, Span, Log)
- `internal/storage/repository.go` - Basic CRUD operations
- `internal/storage/repository_v2.go` - Advanced queries and metrics
- `internal/realtime/hub.go` - Buffered log streaming
- `internal/realtime/events_ws.go` - Live mode snapshots
- `internal/ingest/otlp.go` - OTLP protocol handling
- `internal/ai/service.go` - AI log analysis

**Frontend:**
- `web/src/main.tsx` - React entry point
- `web/src/App.tsx` - Root component
- `web/src/contexts/LiveModeContext.tsx` - Live mode state
- `web/src/layouts/AppLayout.tsx` - Main layout and navigation
- `web/src/features/dashboard/Dashboard.tsx` - Dashboard page
- `web/src/features/logs/LogExplorer.tsx` - Log viewer
- `web/src/features/traces/TraceExplorer.tsx` - Trace viewer
- `web/src/features/topology/ServiceMap.tsx` - Service topology

### Common Commands

```bash
# Development
go run main.go                    # Run backend
cd web && npm run dev             # Run frontend dev server

# Build
cd web && npm run build           # Build frontend
go build -o OtelContext main.go         # Build binary

# Database
sqlite3 OtelContext.db                  # Open SQLite database
mysql -u root -p OtelContext            # Connect to MySQL

# Testing
grpcurl -plaintext localhost:4317 list  # List gRPC services
curl http://localhost:8080/api/health   # Health check

# Monitoring
curl http://localhost:8080/metrics      # Prometheus metrics
```

### Port Reference

- **8080** - HTTP API + WebSocket + Frontend
- **4317** - gRPC OTLP Receiver
- **5173** - Vite dev server (development only)

---

## 🎯 Summary for AI Tools

**When working on Project OtelContext, remember:**

1. ✅ **DO:**
   - Use Go net/http for HTTP
   - Use Mantine UI for frontend
   - Maintain single binary architecture
   - Follow repository pattern
   - Use buffered WebSocket for streaming
   - Write to React Query cache in live mode
   - Handle errors explicitly
   - Add Prometheus metrics for new features

2. ❌ **DON'T:**
   - Use Express.js or other HTTP frameworks
   - Use Tailwind CSS or custom CSS frameworks
   - Split into microservices
   - Manually add tenant filters everywhere
   - Assume public internet access
   - Block ingestion pipeline with slow operations
   - Ignore error handling
   - Skip testing with multiple databases

3. 🎯 **Key Patterns:**
   - Repository pattern for data access
   - Callback pattern for real-time updates
   - Worker pool for concurrent processing
   - Buffering for high-throughput streaming
   - Per-client filtering for live mode
   - DLQ for resilience

4. 📚 **Reference Points:**
   - Architecture diagram (page 2)
   - Data flow diagrams (pages 3-4)
   - API endpoints (pages 11-12)
   - Configuration (pages 16-17)
   - Design decisions (pages 26-28)

---

**End of AI_PROJECT_SPEC.md**

*This document serves as the comprehensive reference for all AI tools and developers working on Project OtelContext. Keep it updated as the project evolves.*

