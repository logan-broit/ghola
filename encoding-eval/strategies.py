"""Reference encoder strategies for the Tier 1 encoding eval harness.

Each strategy takes (session_text, turns, model) and returns a list of
L2-normalized turn embeddings in the same order as turns. Turn spans use
character offsets into session_text.

Strategies registered here:
    isolated                 -- encode each turn text standalone (baseline)
    late-chunk-last-token    -- full-session forward pass + last-token pool
    late-chunk-mean-pool     -- ablation, wrong pooling for Qwen3
    sliding-window-last-token -- long-session variant, 32K window 50% stride

Add new strategies via register_strategy(...).
"""
from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

import torch
from sentence_transformers import SentenceTransformer


# --- Types ----------------------------------------------------------------

Turn = Dict[str, Any]  # {role, content, char_start, char_end}

# A strategy function receives (session_text, turns, model) and returns
# a tensor of shape (n_turns, dim) with each row L2-normalized.
EncodeFn = Callable[[str, List[Turn], SentenceTransformer], torch.Tensor]


@dataclass
class EncoderStrategy:
    name: str
    encode_fn: EncodeFn
    model_factory: Callable[[], SentenceTransformer]
    metadata: Dict[str, Any] = field(default_factory=dict)
    _model: Optional[SentenceTransformer] = None

    def model(self) -> SentenceTransformer:
        if self._model is None:
            self._model = self.model_factory()
        return self._model


# --- Registry -------------------------------------------------------------

_REGISTRY: Dict[str, EncoderStrategy] = {}


def register_strategy(
    name: str,
    encode_fn: EncodeFn,
    model_factory: Callable[[], SentenceTransformer],
    metadata: Optional[Dict[str, Any]] = None,
) -> None:
    if name in _REGISTRY:
        raise ValueError(f"strategy '{name}' already registered")
    _REGISTRY[name] = EncoderStrategy(
        name=name,
        encode_fn=encode_fn,
        model_factory=model_factory,
        metadata=metadata or {},
    )


def get_strategy(name: str) -> EncoderStrategy:
    if name not in _REGISTRY:
        raise KeyError(
            f"unknown strategy '{name}'; registered: {sorted(_REGISTRY.keys())}"
        )
    return _REGISTRY[name]


def list_strategies() -> List[str]:
    return sorted(_REGISTRY.keys())


# --- Helpers --------------------------------------------------------------

def _default_model_factory() -> SentenceTransformer:
    """Qwen3-Embedding-0.6B, the pinned model matching Phase 2 production."""
    return SentenceTransformer("Qwen/Qwen3-Embedding-0.6B")


def _l2_normalize(v: torch.Tensor) -> torch.Tensor:
    n = torch.linalg.norm(v)
    if n == 0 or not torch.isfinite(n):
        return v
    return v / n


def _last_real_token_idx_for_span(
    offsets: List[Tuple[int, int]],
    mask: torch.Tensor,
    char_start: int,
    char_end: int,
) -> int:
    """Return the index of the LAST real token whose char-offset overlaps
    [char_start, char_end). Skips special tokens (offset == (0, 0)) and
    padding tokens (mask == 0). Raises if no overlap found.
    """
    last = -1
    for i, (a, b) in enumerate(offsets):
        if not bool(mask[i].item()):
            continue
        if a == 0 and b == 0:
            continue  # special token (bos/eos)
        if a < char_end and b > char_start:
            last = i
    if last < 0:
        raise ValueError(
            f"no overlapping tokens for char span [{char_start}, {char_end})"
        )
    return last


def _first_real_token_idx_for_span(
    offsets: List[Tuple[int, int]],
    mask: torch.Tensor,
    char_start: int,
    char_end: int,
) -> int:
    for i, (a, b) in enumerate(offsets):
        if not bool(mask[i].item()):
            continue
        if a == 0 and b == 0:
            continue
        if a < char_end and b > char_start:
            return i
    raise ValueError(
        f"no overlapping tokens for char span [{char_start}, {char_end})"
    )


# --- Strategy: isolated (baseline) ---------------------------------------

def encode_isolated(
    session_text: str,
    turns: List[Turn],
    model: SentenceTransformer,
) -> torch.Tensor:
    """Encode each turn's content alone via the model's native sentence
    encoding. No session context. This is the baseline to beat."""
    texts = [t["content"] for t in turns]
    embs = model.encode(
        texts,
        normalize_embeddings=True,
        convert_to_tensor=True,
    )
    if embs.ndim == 1:
        embs = embs.unsqueeze(0)
    return embs


# --- Strategy: late-chunk-last-token (candidate) -------------------------

def encode_late_chunk_last_token(
    session_text: str,
    turns: List[Turn],
    model: SentenceTransformer,
) -> torch.Tensor:
    """Full-session forward pass. For each turn, extract the hidden state at
    the LAST real token whose char-offset overlaps the turn's span, then
    L2-normalize. Qwen3-Embedding uses last-token pooling natively, so turn
    embeddings live in the same representation space as query embeddings.

    Causal attention means turn N's last-token embedding has attended to
    every token in turns 0..N plus its own span. This is the context-
    awareness that naive per-turn encoding cannot provide.

    Falls back to sliding window when n_tokens > model.max_seq_length.
    """
    enc = model.tokenizer(
        session_text,
        return_tensors="pt",
        return_offsets_mapping=True,
        truncation=False,
        add_special_tokens=True,
    )
    n_tokens = int(enc["input_ids"].shape[1])
    if n_tokens > model.max_seq_length:
        return encode_sliding_window_last_token(session_text, turns, model)

    toks = model.encode(
        session_text,
        output_value="token_embeddings",
        convert_to_tensor=True,
    )
    offsets: List[Tuple[int, int]] = [tuple(x) for x in enc["offset_mapping"][0].tolist()]
    mask = enc["attention_mask"][0].to(toks.device)

    vecs = []
    for t in turns:
        idx = _last_real_token_idx_for_span(offsets, mask, t["char_start"], t["char_end"])
        vecs.append(_l2_normalize(toks[idx].float()))
    return torch.stack(vecs)


# --- Strategy: late-chunk-mean-pool (ablation) ---------------------------

def encode_late_chunk_mean_pool(
    session_text: str,
    turns: List[Turn],
    model: SentenceTransformer,
) -> torch.Tensor:
    """Same single-pass encoding as late-chunk-last-token, but mean-pool the
    turn's tokens instead of taking the last one. This is the WRONG pooling
    for Qwen3-Embedding (research probe showed ~0.72 cosine vs native
    last-token output). Kept as an ablation control -- expect it to
    underperform the last-token variant."""
    enc = model.tokenizer(
        session_text,
        return_tensors="pt",
        return_offsets_mapping=True,
        truncation=False,
        add_special_tokens=True,
    )
    n_tokens = int(enc["input_ids"].shape[1])
    if n_tokens > model.max_seq_length:
        raise NotImplementedError(
            "mean-pool sliding window not implemented; this strategy is "
            "an ablation only, don't feed it long sessions"
        )

    toks = model.encode(
        session_text,
        output_value="token_embeddings",
        convert_to_tensor=True,
    ).float()
    offsets: List[Tuple[int, int]] = [tuple(x) for x in enc["offset_mapping"][0].tolist()]
    mask = enc["attention_mask"][0].to(toks.device)

    vecs = []
    for t in turns:
        first = _first_real_token_idx_for_span(offsets, mask, t["char_start"], t["char_end"])
        last = _last_real_token_idx_for_span(offsets, mask, t["char_start"], t["char_end"])
        span = toks[first : last + 1]
        pooled = span.mean(dim=0)
        vecs.append(_l2_normalize(pooled))
    return torch.stack(vecs)


# --- Strategy: sliding-window-last-token (long-session variant) ----------

def encode_sliding_window_last_token(
    session_text: str,
    turns: List[Turn],
    model: SentenceTransformer,
    stride_frac: float = 0.5,
) -> torch.Tensor:
    """Sliding window variant for sessions exceeding model.max_seq_length.
    Windows of model.max_seq_length tokens with stride_frac overlap. For
    each turn, select the window where the turn is most centrally
    positioned (maximizes symmetric context), then take the last-token
    hidden state of the turn's span within that window."""
    tokenizer = model.tokenizer
    full_enc = tokenizer(
        session_text,
        return_tensors="pt",
        return_offsets_mapping=True,
        truncation=False,
        add_special_tokens=False,
    )
    full_offsets: List[Tuple[int, int]] = [
        tuple(x) for x in full_enc["offset_mapping"][0].tolist()
    ]
    n_tokens = len(full_offsets)
    window_size = model.max_seq_length
    stride = max(1, int(window_size * stride_frac))

    # Map each turn to its full-session token range
    turn_token_ranges: List[Tuple[int, int]] = []
    for t in turns:
        first, last = None, None
        for i, (a, b) in enumerate(full_offsets):
            if a == 0 and b == 0:
                continue
            if a < t["char_end"] and b > t["char_start"]:
                if first is None:
                    first = i
                last = i
        if first is None or last is None:
            raise ValueError(
                f"turn at char[{t['char_start']}:{t['char_end']}] has no tokens"
            )
        turn_token_ranges.append((first, last))

    # Construct windows: [start_token, end_token] (exclusive end)
    windows: List[Tuple[int, int]] = []
    cursor = 0
    while cursor < n_tokens:
        end = min(cursor + window_size, n_tokens)
        windows.append((cursor, end))
        if end >= n_tokens:
            break
        cursor += stride

    def decode_window(ws: int, we: int) -> str:
        # Use offsets to slice session_text for the window's char range.
        # Guard: skip leading/trailing special-token offsets that are (0, 0)
        # within the window range.
        char_start = None
        char_end = None
        for i in range(ws, we):
            a, b = full_offsets[i]
            if a == 0 and b == 0:
                continue
            if char_start is None:
                char_start = a
            char_end = b
        if char_start is None:
            return ""
        return session_text[char_start:char_end]

    # Encode each window once, cache token embeddings
    window_cache: List[Tuple[torch.Tensor, List[Tuple[int, int]], torch.Tensor, int]] = []
    # each entry: (token_embs, window_offsets, mask, window_char_start)
    for ws, we in windows:
        text = decode_window(ws, we)
        if not text:
            window_cache.append((torch.empty(0), [], torch.empty(0), 0))
            continue
        w_enc = tokenizer(
            text,
            return_tensors="pt",
            return_offsets_mapping=True,
            truncation=True,
            max_length=window_size,
            add_special_tokens=True,
        )
        w_toks = model.encode(
            text,
            output_value="token_embeddings",
            convert_to_tensor=True,
        ).float()
        w_offsets = [tuple(x) for x in w_enc["offset_mapping"][0].tolist()]
        w_mask = w_enc["attention_mask"][0].to(w_toks.device)
        # window char start (first real token's a)
        w_char_start = next(
            (a for (a, b) in (full_offsets[ws : we])
             if not (a == 0 and b == 0)),
            0,
        )
        window_cache.append((w_toks, w_offsets, w_mask, w_char_start))

    def best_window_for_turn(first: int, last: int) -> int:
        """Pick the window index where (first, last) is most centrally
        positioned. Prefer windows that fully contain the turn; among
        those, maximize min(turn_start - window_start,
        window_end - turn_end)."""
        best_idx = None
        best_margin = -math.inf
        for wi, (ws, we) in enumerate(windows):
            if first < ws or last >= we:
                continue
            margin = min(first - ws, we - last)
            if margin > best_margin:
                best_margin = margin
                best_idx = wi
        if best_idx is None:
            raise ValueError(
                f"no window fully contains turn tokens [{first}, {last}]; "
                f"turn may exceed window_size={window_size}"
            )
        return best_idx

    vecs = []
    for t, (first, last) in zip(turns, turn_token_ranges):
        wi = best_window_for_turn(first, last)
        w_toks, w_offsets, w_mask, w_char_start = window_cache[wi]
        # Translate turn char span into window-local char coordinates
        local_start = t["char_start"] - w_char_start
        local_end = t["char_end"] - w_char_start
        idx = _last_real_token_idx_for_span(
            w_offsets, w_mask, local_start, local_end
        )
        vecs.append(_l2_normalize(w_toks[idx]))

    return torch.stack(vecs)


# --- Register defaults ---------------------------------------------------

register_strategy(
    name="isolated",
    encode_fn=encode_isolated,
    model_factory=_default_model_factory,
    metadata={
        "pooling": "native-last-token",
        "context": "none",
        "purpose": "baseline",
    },
)

register_strategy(
    name="late-chunk-last-token",
    encode_fn=encode_late_chunk_last_token,
    model_factory=_default_model_factory,
    metadata={
        "pooling": "last-token",
        "context": "full-session",
        "window_mode": "single-pass-or-slide",
        "purpose": "candidate",
    },
)

register_strategy(
    name="late-chunk-mean-pool",
    encode_fn=encode_late_chunk_mean_pool,
    model_factory=_default_model_factory,
    metadata={
        "pooling": "mean-pool",
        "context": "full-session",
        "window_mode": "single-pass-only",
        "purpose": "ablation (wrong-pooling control)",
    },
)

register_strategy(
    name="sliding-window-last-token",
    encode_fn=encode_sliding_window_last_token,
    model_factory=_default_model_factory,
    metadata={
        "pooling": "last-token",
        "context": "window-scoped",
        "window_mode": "sliding",
        "stride_frac": 0.5,
        "purpose": "long-session variant (auto-dispatched by late-chunk-last-token)",
    },
)
