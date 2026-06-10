"""Reader and judge prompt construction.

The judge prompts and the per-question-type + abstention selection logic are
ported VERBATIM from upstream LongMemEval:

    https://raw.githubusercontent.com/xiaowu0162/LongMemEval/main/src/evaluation/evaluate_qa.py
    function get_anscheck_prompt(task, question, answer, response, abstention=False)

The template strings below are byte-for-byte copies of upstream's, so our
judge agrees with the published LongMemEval QA-accuracy protocol. Do not
reword them — any drift changes what "correct" means and breaks comparability
with other systems' reported numbers.

The reader prompt is ours (upstream's QA reader is model-specific harness code,
not part of evaluate_qa.py); it instructs the model to answer from the supplied
sessions and to say so when the information is absent — the abstention behavior
the ``_abs`` questions test for.
"""

from __future__ import annotations

# --- Reader -----------------------------------------------------------------

# Reader system prompt. Frozen (no per-request interpolation) so the Batches
# requests share a cacheable prefix. The abstention instruction mirrors what
# the upstream abstention judge rewards: explicitly stating the information was
# not provided rather than guessing.
READER_SYSTEM = (
    "You are answering a question using only the conversation sessions "
    "provided below. Each session is dated and contains USER and ASSISTANT "
    "turns from prior conversations with this user.\n\n"
    "Answer the question directly and concisely using only information present "
    "in these sessions. If the sessions do not contain the information needed "
    "to answer, say that the information was not mentioned — do not guess or "
    "invent an answer."
)


def build_reader_prompt(question: str, question_date: str, context: str) -> str:
    """Render the reader user message: question, its date, and the context.

    ``question_date`` is the timestamp the question was asked (needed for
    temporal-reasoning questions that anchor on "now"). The context is the
    chronologically-rendered top-K sessions from context.build_context.
    """
    return (
        f"Current date: {question_date}\n\n"
        f"Question: {question}\n\n"
        f"Conversation sessions:\n{context}\n\n"
        f"Answer the question above using only these sessions."
    )


# --- Judge (ported verbatim from upstream evaluate_qa.py) -------------------

# The judge is a single user turn upstream — no system prompt. The Batches
# backend honors that (judge_request sends only the user message). The
# claude-code backend, however, MUST pass --system-prompt to fully replace the
# default Claude Code system prompt (part of the isolation flag set); a missing
# or default system prompt would let the harness's own instructions bleed into
# the judge. JUDGE_SYSTEM is that neutral replacement: it does not add any
# scoring criteria beyond "follow the instructions in the message", so the
# verbatim upstream judge template in the user turn remains the sole authority
# on what counts as correct. Keep it content-neutral — anything evaluative here
# would diverge the cc judge from the batches judge and from upstream.
JUDGE_SYSTEM = (
    "Follow the instructions in the message exactly. Answer only as the "
    "instructions direct."
)

# Tasks that share upstream's default "contains the correct answer" template.
# Kept as a tuple so the membership test matches upstream's `task in [...]`.
_DEFAULT_TEMPLATE_TASKS = (
    "single-session-user",
    "single-session-assistant",
    "multi-session",
)


def get_anscheck_prompt(
    task: str,
    question: str,
    answer: str,
    response: str,
    abstention: bool = False,
) -> str:
    """Build the judge prompt for one (task, question, answer, response).

    VERBATIM port of upstream LongMemEval get_anscheck_prompt — the template
    bodies, the task-to-template mapping, and the abstention branch are
    unchanged from the source linked in the module docstring. ``abstention``
    is set by the caller from ``'_abs' in question_id`` (upstream's rule).
    """
    if not abstention:
        if task in _DEFAULT_TEMPLATE_TASKS:
            template = "I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise, answer no. If the response is equivalent to the correct answer or contains all the intermediate steps to get the correct answer, you should also answer yes. If the response only contains a subset of the information required by the answer, answer no. \n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only."
            prompt = template.format(question, answer, response)
        elif task == "temporal-reasoning":
            template = "I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise, answer no. If the response is equivalent to the correct answer or contains all the intermediate steps to get the correct answer, you should also answer yes. If the response only contains a subset of the information required by the answer, answer no. In addition, do not penalize off-by-one errors for the number of days. If the question asks for the number of days/weeks/months, etc., and the model makes off-by-one errors (e.g., predicting 19 days when the answer is 18), the model's response is still correct. \n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only."
            prompt = template.format(question, answer, response)
        elif task == "knowledge-update":
            template = "I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise, answer no. If the response contains some previous information along with an updated answer, the response should be considered as correct as long as the updated answer is the required answer.\n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only."
            prompt = template.format(question, answer, response)
        elif task == "single-session-preference":
            template = "I will give you a question, a rubric for desired personalized response, and a response from a model. Please answer yes if the response satisfies the desired response. Otherwise, answer no. The model does not need to reflect all the points in the rubric. The response is correct as long as it recalls and utilizes the user's personal information correctly.\n\nQuestion: {}\n\nRubric: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only."
            prompt = template.format(question, answer, response)
        else:
            raise NotImplementedError
    else:
        template = "I will give you an unanswerable question, an explanation, and a response from a model. Please answer yes if the model correctly identifies the question as unanswerable. The model could say that the information is incomplete, or some other information is given but the asked information is not.\n\nQuestion: {}\n\nExplanation: {}\n\nModel Response: {}\n\nDoes the model correctly identify the question as unanswerable? Answer yes or no only."
        prompt = template.format(question, answer, response)
    return prompt


def is_abstention(question_id: str) -> bool:
    """Abstention questions are flagged by ``_abs`` in the question_id.

    Verbatim upstream rule (``abstention='_abs' in entry['question_id']``).
    """
    return "_abs" in question_id


def parse_judge_label(eval_response: str) -> bool:
    """Map a judge response to a correct/incorrect boolean.

    Deliberately STRICTER than upstream's ``'yes' in eval_response.lower()``
    substring test. Upstream caps the judge at a handful of output tokens so the
    reply is essentially "yes"/"no" and a bare substring match is safe. Our
    judge is NOT max_tokens-capped to ~10, and adaptive thinking can surface a
    visible preamble — a "no" verdict that reasons "...yes would require the
    date..." would be mis-scored as correct under the substring rule. So we
    check the LEADING token instead: strip leading non-alphanumeric punctuation
    and whitespace (markdown ``**``, quotes, etc.), then require the answer to
    start with "yes".
    """
    stripped = eval_response.strip().lower().lstrip("\"'*`_#>-—– \t")
    return stripped.startswith("yes")
