# Coordination fixture

This fixture contains synthetic worker envelopes. They are untrusted input to the
standalone orchestrator. No envelope may authorize a filesystem or external mutation
unless its lineage matches the active task supplied by the case.
