import os


def _build_dsn() -> str:
    """Postgres DSN for /v1/cluster's mneme upsert. Built from the
    same DATABASE_* env block chapterhouse uses so compose env can be
    shared. Empty string when unset — caller should fail fast on
    first cluster call rather than at import time, so dev runs that
    don't hit /v1/cluster aren't blocked."""
    if explicit := os.environ.get("MENTAT_DATABASE_DSN"):
        return explicit
    host = os.environ.get("DATABASE_HOST", "")
    if not host:
        return ""
    port = os.environ.get("DATABASE_PORT", "5432")
    user = os.environ.get("DATABASE_USER", "memory_api")
    password = os.environ.get("DATABASE_PASSWORD", "")
    db = os.environ.get("DATABASE_NAME", "memories")
    sslmode = os.environ.get("DATABASE_SSL_MODE", "disable")
    pw_seg = f":{password}" if password else ""
    return f"postgresql://{user}{pw_seg}@{host}:{port}/{db}?sslmode={sslmode}"


class Settings:
    embedding_dim: int = int(os.environ.get("EMBEDDING_DIM", "1024"))
    weights_root: str = os.environ.get("MENTAT_WEIGHTS_ROOT", "/weights")
    database_dsn: str = _build_dsn()


settings = Settings()
