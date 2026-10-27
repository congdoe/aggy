-- aggy index schema, version 3: probes are looked up per CLI path and
-- version, to reuse a probe until the version changes.
CREATE INDEX probes_cache ON probes (vendor, cli_path, version, probed_at);
