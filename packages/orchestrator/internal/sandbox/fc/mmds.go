package fc

// The metadata serialization should not be changed — it is different from the field names we use here!
type MmdsMetadata struct {
    SandboxId            string `json:"instanceID"`
    TemplateId           string `json:"envID"`
    LogsCollectorAddress string `json:"address"`
    TraceId              string `json:"traceID"`
    TeamId               string `json:"teamID"`
    // Optional business identifiers and EFS config for runtime mounts
    UserId               string `json:"userID,omitempty"`
    EfsHost              string `json:"efsHost,omitempty"`
    EfsRoot              string `json:"efsRoot,omitempty"`
}
