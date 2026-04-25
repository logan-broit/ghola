"""Stage 1 pooler: deterministic type-weighted mean.

This is the cold-start path and the reference any trained pooler must
beat. It's also the production path until PR8's AttentionPool ships.
Zero trainable parameters.
"""
import torch

from .schemas import Event

TYPE_WEIGHTS: dict[str, float] = {
    "user":        1.0,
    "assistant":   0.5,
    "tool_result": 0.1,
    "system":      0.0,
}


def type_weighted_mean_pool(events: list[Event]) -> list[float]:
    weights = torch.tensor(
        [TYPE_WEIGHTS.get(e.type, 0.0) for e in events], dtype=torch.float32
    )
    embs = torch.tensor([e.embedding for e in events], dtype=torch.float32)
    wsum = weights.sum()
    if wsum.item() == 0.0:
        # All events were system-type. Fall back to uniform rather than
        # returning a 0-vector that would poison HNSW.
        weights = torch.ones_like(weights)
        wsum = weights.sum()
    pooled = (weights.unsqueeze(1) * embs).sum(dim=0) / wsum
    norm = torch.linalg.vector_norm(pooled).clamp(min=1e-12)
    return (pooled / norm).tolist()
