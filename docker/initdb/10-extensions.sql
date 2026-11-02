-- Basalt 运行所需的两个 PostgreSQL 扩展；pgvector 镜像已带二进制，首启只建扩展。
-- 幂等：重复执行不报错；应用启动时另有 CheckExtensions 门禁，缺扩展拒绝启动。
-- The two extensions Basalt requires. The pgvector image ships the binaries; this
-- script only creates the extensions. Idempotent; the app re-checks at startup.
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
