import os


class Settings:
    embedding_dim: int = int(os.environ.get("EMBEDDING_DIM", "1024"))
    weights_root: str = os.environ.get("MENTAT_WEIGHTS_ROOT", "/weights")
    database_dsn: str | None = os.environ.get("MENTAT_DATABASE_DSN")
    melange_url: str = os.environ.get("MELANGE_URL", "http://melange:8082")


settings = Settings()
