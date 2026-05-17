import os


class Settings:
    model: str = os.environ.get("TRUTHSAYER_MODEL", "BAAI/bge-reranker-base")
    device: str = os.environ.get("TRUTHSAYER_DEVICE", "cpu")
    # Torch dtype for model weights + activations. fp16 cuts the bge-reranker-v2-m3
    # footprint from ~12 GB to ~3-4 GB on cuda with no measurable quality delta.
    # Override to float32 if you ever need to A/B against the old behavior.
    dtype: str = os.environ.get("TRUTHSAYER_DTYPE", "float16")
    max_length: int = int(os.environ.get("TRUTHSAYER_MAX_LENGTH", "512"))
    # chunk_chars sizes the windows we slide over a long candidate text
    # before scoring. Each chunk gets a forward pass; we max-pool the
    # scores per candidate. 1500 matches the LongMemEval bench backend
    # so per-session scoring shape lines up. Texts shorter than the
    # window go through whole, no chunking overhead.
    chunk_chars: int = int(os.environ.get("TRUTHSAYER_CHUNK_CHARS", "1500"))
    port: int = int(os.environ.get("TRUTHSAYER_PORT", "8085"))


settings = Settings()
