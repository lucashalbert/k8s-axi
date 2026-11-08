package kube

// logsSupported reports whether the logs command can read resource r. The
// real implementation arrives with the logs command.
func logsSupported(r Resource) bool { return r.GVR.Group == "" && r.GVR.Resource == "pods" }
