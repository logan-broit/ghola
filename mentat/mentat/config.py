import os


class Settings:
    embedding_dim: int = int(os.environ.get("EMBEDDING_DIM", "1024"))
    weights_root: str = os.environ.get("MENTAT_WEIGHTS_ROOT", "/weights")


settings = Settings()
