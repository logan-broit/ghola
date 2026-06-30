from __future__ import annotations

from lme_qa.stats import tokenize_words, idf_map, self_information, BM25Scorer


def test_tokenize_words_lowercases_and_splits():
    assert tokenize_words("The Mauve-DB, port 9931!") == [
        "the",
        "mauve",
        "db",
        "port",
        "9931",
    ]


def test_idf_rare_token_scores_higher_than_common():
    docs = ["the cat sat", "the dog sat", "the bird flew", "quantum entanglement"]
    idf = idf_map(docs)
    assert idf["quantum"] > idf["the"]


def test_self_information_sums_idf_of_span_tokens():
    idf = {"quantum": 3.0, "the": 0.1}
    # unknown tokens fall back to max idf; here all tokens known
    assert abs(self_information("the quantum the", idf) - 3.2) < 1e-9


def test_bm25_ranks_query_term_doc_first():
    scorer = BM25Scorer()
    scores = scorer("port number", [("a", "the cat sat"), ("b", "what port number please")])
    assert scores["b"] > scores["a"]


def test_bm25_empty_items_returns_empty():
    assert BM25Scorer()("q", []) == {}
