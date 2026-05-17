"""Cross-encoder scorer wrapping sentence-transformers' CrossEncoder.

The score scale is whatever the underlying model produces — bge-reranker
returns a sigmoid-ish [0, 1], some others return raw logits. The HTTP
client normalizes per-request before fusion, so this layer doesn't try
to rescale.

For long candidate texts (full session conversations), we chunk at
TRUTHSAYER_CHUNK_CHARS and take the max score across chunks per
candidate — matches the LongMemEval bench backend's strategy. The
underlying model truncates beyond its max_length token limit on each
forward pass, so without chunking, late turns in a long session never
see the cross-encoder. Max-pooling across chunks recovers that signal.
"""
from __future__ import annotations

import torch
from sentence_transformers import CrossEncoder


_DTYPE_MAP = {
    "float16": torch.float16,
    "fp16": torch.float16,
    "half": torch.float16,
    "bfloat16": torch.bfloat16,
    "bf16": torch.bfloat16,
    "float32": torch.float32,
    "fp32": torch.float32,
    "float": torch.float32,
}


class Scorer:
    def __init__(
        self,
        model_name: str,
        device: str,
        max_length: int,
        chunk_chars: int = 1500,
        dtype: str = "float16",
    ) -> None:
        torch_dtype = _DTYPE_MAP.get(dtype.lower(), torch.float16)
        # On cpu, fp16 is slower than fp32 and not what the user wants — only
        # apply the dtype on cuda devices.
        model_kwargs = {"torch_dtype": torch_dtype} if device.startswith("cuda") else {}
        self._model = CrossEncoder(
            model_name,
            device=device,
            max_length=max_length,
            model_kwargs=model_kwargs,
        )
        self._model_name = model_name
        self._device = device
        self._chunk_chars = chunk_chars

    @property
    def model_name(self) -> str:
        return self._model_name

    @property
    def device(self) -> str:
        return self._device

    def score(self, query: str, texts: list[str]) -> list[float]:
        # Build (sid_idx, query, chunk) triples. sid_idx tracks which
        # candidate each chunk came from so we can max-pool back per
        # candidate after the model run. Single batched forward pass
        # over all chunks across all candidates — far cheaper than
        # one call per candidate.
        owners: list[int] = []
        pairs: list[tuple[str, str]] = []
        for i, text in enumerate(texts):
            for chunk in self._chunks(text):
                owners.append(i)
                pairs.append((query, chunk))
        if not pairs:
            return [0.0] * len(texts)
        raw = self._model.predict(pairs, show_progress_bar=False)
        # Max per candidate. Initialize to -inf so the first chunk
        # always wins; replace with 0.0 below for candidates that
        # produced no chunks (empty text).
        out = [float("-inf")] * len(texts)
        for owner, score in zip(owners, raw):
            f = float(score)
            if f > out[owner]:
                out[owner] = f
        return [(s if s != float("-inf") else 0.0) for s in out]

    def _chunks(self, text: str) -> list[str]:
        if not text:
            return []
        if len(text) <= self._chunk_chars:
            return [text]
        return [text[i : i + self._chunk_chars] for i in range(0, len(text), self._chunk_chars)]
