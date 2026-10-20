package storage

// TopologyEntity is either a resource identity (Source/Target empty) or an
// observed directed call between two resource identities. These are a bounded
// visualization projection, independent of raw exemplar retention.
type TopologyEntity struct {
	ID          string `gorm:"primaryKey;size:64" json:"id"`
	TenantID    string `gorm:"not null;size:128;index:idx_topology_entities_tenant,priority:1" json:"-"`
	Environment string `gorm:"not null;size:255;index:idx_topology_entities_tenant,priority:2" json:"environment"`
	Namespace   string `gorm:"not null;size:255" json:"namespace"`
	Service     string `gorm:"not null;size:255" json:"service"`
	Instance    string `gorm:"not null;size:255" json:"instance"`
	Host        string `gorm:"not null;size:255" json:"host"`
	Workload    string `gorm:"not null;size:255" json:"workload"`
	Identity    string `gorm:"not null;size:32" json:"identity"`
	Source      string `gorm:"not null;size:64" json:"source,omitempty"`
	Target      string `gorm:"not null;size:64" json:"target,omitempty"`
	Signals     int64  `gorm:"not null" json:"signals"`
	FirstSeen   int64  `gorm:"not null" json:"first_seen"`
	LastSeen    int64  `gorm:"not null;index:idx_topology_entities_last_seen" json:"last_seen"`
}

func (TopologyEntity) TableName() string { return "topology_entities" }

// Five-minute rows retain observed span/request/call counts, not raw traces.
// Integers keep flush subtraction and retry arithmetic exact.
type TopologyBucket struct {
	EntityID       string `gorm:"primaryKey;size:64"`
	WindowStart    int64  `gorm:"primaryKey;index:idx_topology_buckets_window"`
	Spans          int64  `gorm:"not null"`
	Requests       int64  `gorm:"not null"`
	Errors         int64  `gorm:"not null"`
	DurationMicros int64  `gorm:"not null"`
}

func (TopologyBucket) TableName() string { return "topology_buckets" }
