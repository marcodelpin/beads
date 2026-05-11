CREATE INDEX IF NOT EXISTS idx_wisp_dep_type ON wisp_dependencies (type);
CREATE INDEX IF NOT EXISTS idx_wisp_dep_type_depends ON wisp_dependencies (type, depends_on_id);
