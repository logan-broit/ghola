"""Cold-start predictor: L1_{t+1} := L1_t.

Identity is the correct fallback before Stage B training has produced
weights. PR6 replaces this when weights are present.
"""


def identity_predict(history: list[list[float]]) -> list[float]:
    if not history:
        raise ValueError("history must be non-empty")
    return list(history[-1])
