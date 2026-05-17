"""Generator for eval_cases.jsonl.

Declaring cases with char offsets by hand is error-prone (we've verified
this empirically). Instead, this script declares each case as a list of
(role, content) pairs plus a query + target position; the helper computes
char offsets and writes a valid JSONL file.

Run me:
    python build_cases.py
    (writes eval_cases.jsonl)

Categories:
    identity-baseline  -- canary, trivial exact-ish match
    self-contained     -- target turn meaningful standalone
    back-reference     -- target turn implicitly references earlier content;
                          late chunking should win
    forward-reference  -- query matches context only established later.
                          Qwen3 is a causal decoder; late chunking should NOT
                          help here (sanity check that it doesn't magically
                          improve cases where context flows the wrong way).
    short-session      -- 1-2 turns
    multi-topic        -- session covers distinct topics, query targets one

Note: long-session (>32K tokens) is NOT covered here because hand-curating
a realistic >32K-token conversation is impractical. Long-session cases will
come from LongMemEval samples once we have a batch-adapter.
"""
from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Dict, List, Tuple


def build_case(
    case_id: str,
    category: str,
    turns_raw: List[Tuple[str, str]],  # [(role, content), ...]
    query: str,
    target_position: int,
    notes: str = "",
    secondary_positions: List[int] | None = None,
) -> Dict[str, Any]:
    """Construct a case with correct char offsets from raw (role, content)
    pairs. Concatenating all contents in order produces session_text.
    """
    session_parts: List[str] = []
    turns: List[Dict[str, Any]] = []
    cursor = 0
    for role, content in turns_raw:
        start = cursor
        end = cursor + len(content)
        turns.append({
            "role": role,
            "content": content,
            "char_start": start,
            "char_end": end,
        })
        session_parts.append(content)
        cursor = end

    session_text = "".join(session_parts)
    assert 0 <= target_position < len(turns), (
        f"target_position {target_position} out of range for {len(turns)} turns"
    )
    return {
        "id": case_id,
        "category": category,
        "notes": notes,
        "session_text": session_text,
        "turns": turns,
        "query": query,
        "target_position": target_position,
        "secondary_positions": secondary_positions or [],
    }


CASES: List[Dict[str, Any]] = []


def add(case: Dict[str, Any]) -> None:
    CASES.append(case)


# ─────────────────────────────────────────────────────────────────────────
# identity-baseline  (canaries; any broken encoder should fail these)
# ─────────────────────────────────────────────────────────────────────────

add(build_case(
    case_id="identity-exact-substring-01",
    category="identity-baseline",
    turns_raw=[
        ("user", "We agreed to meet at 3pm on Thursday in the main conference room."),
        ("assistant", " I'll bring the printed slides."),
    ],
    query="what time are we meeting on Thursday",
    target_position=0,
    notes="Query is a near-exact substring of the target turn.",
))

add(build_case(
    case_id="identity-direct-answer-01",
    category="identity-baseline",
    turns_raw=[
        ("user", "USER: What's the freezing point of water at sea level?"),
        ("assistant", "\nASSISTANT: 0 degrees Celsius, or 32 degrees Fahrenheit."),
    ],
    query="what is the freezing point of water",
    target_position=1,
    notes="Direct Q/A; answer in the assistant turn.",
))

add(build_case(
    case_id="identity-api-key-01",
    category="identity-baseline",
    turns_raw=[
        ("system", "System prompt: you are a helpful developer assistant."),
        ("user", "\nUSER: My staging Stripe API key is sk_test_abc123def456."),
        ("assistant", "\nASSISTANT: Noted, I won't log it. What do you need help with?"),
    ],
    query="what is my staging stripe api key",
    target_position=1,
    notes="Specific token in turn 1; any working encoder finds it.",
))

add(build_case(
    case_id="identity-phone-number-01",
    category="identity-baseline",
    turns_raw=[
        ("user", "Save my number: 415-555-0142. Call me if anything changes."),
    ],
    query="what is my phone number",
    target_position=0,
    notes="Single turn; target must be 0 by construction.",
))

add(build_case(
    case_id="identity-cli-flag-01",
    category="identity-baseline",
    turns_raw=[
        ("user", "USER: How do I enable verbose logging in ripgrep?"),
        ("assistant", "\nASSISTANT: Pass --debug or -N for line numbers; there's no single 'verbose' flag, but --stats gives you match summaries."),
    ],
    query="how do I enable verbose logging in ripgrep",
    target_position=1,
    notes="Lexical overlap strong.",
))


# ─────────────────────────────────────────────────────────────────────────
# self-contained  (target turn is meaningful standalone)
# ─────────────────────────────────────────────────────────────────────────

add(build_case(
    case_id="self-contained-k8s-limit-01",
    category="self-contained",
    turns_raw=[
        ("user", "USER: I'm debugging a Kubernetes pod that keeps getting OOMKilled."),
        ("assistant", "\nASSISTANT: What are the current memory limits?"),
        ("user", "\nUSER: Limits are 4Gi, requests are 2Gi. The container runs Qwen3-0.6B for embeddings."),
        ("assistant", "\nASSISTANT: Bump the limit to 8Gi."),
    ],
    query="what are the current memory limits on the container running Qwen3",
    target_position=2,
))

add(build_case(
    case_id="self-contained-restaurant-rec-01",
    category="self-contained",
    turns_raw=[
        ("assistant", "For tonight, try Sushi Nakazawa on West 58th -- best omakase in the city."),
        ("assistant", " They take reservations up to 30 days out."),
        ("assistant", " Tell them you want to sit at the counter."),
    ],
    query="which restaurant did you recommend for tonight",
    target_position=0,
))

add(build_case(
    case_id="self-contained-recipe-temp-01",
    category="self-contained",
    turns_raw=[
        ("user", "USER: I'm baking sourdough for the first time. Preheat temp?"),
        ("assistant", "\nASSISTANT: Preheat your dutch oven at 500F for 45 minutes, then drop to 450F when you load the bread."),
        ("user", "\nUSER: Got it. Scoring tips?"),
        ("assistant", "\nASSISTANT: One confident cut at a 30-degree angle, about a quarter inch deep."),
    ],
    query="what temperature should I preheat the dutch oven for sourdough",
    target_position=1,
))

add(build_case(
    case_id="self-contained-git-command-01",
    category="self-contained",
    turns_raw=[
        ("user", "USER: How do I undo the last commit but keep changes staged?"),
        ("assistant", "\nASSISTANT: git reset --soft HEAD~1 leaves your changes in the index."),
        ("user", "\nUSER: What if I want them unstaged?"),
        ("assistant", "\nASSISTANT: git reset --mixed HEAD~1 (or just git reset HEAD~1, since --mixed is the default)."),
    ],
    query="how do I undo the last commit keeping changes unstaged",
    target_position=3,
))

add(build_case(
    case_id="self-contained-cert-expiry-01",
    category="self-contained",
    turns_raw=[
        ("assistant", "Checked the cert: CN=example.dev, issuer Let's Encrypt R3, not-after 2026-07-12."),
        ("user", " When does it need to be renewed?"),
        ("assistant", " cert-manager handles renewal automatically at 30 days before expiry, so around 2026-06-12."),
    ],
    query="when does the example.dev certificate expire",
    target_position=0,
))

add(build_case(
    case_id="self-contained-budget-01",
    category="self-contained",
    turns_raw=[
        ("user", "Our Q2 infra budget is $45K split across compute, storage, and egress."),
        ("assistant", " What's the breakdown?"),
        ("user", " 60% compute, 25% storage, 15% egress."),
    ],
    query="what is our q2 infra budget",
    target_position=0,
))

add(build_case(
    case_id="self-contained-sql-aggregate-01",
    category="self-contained",
    turns_raw=[
        ("user", "USER: How do I get the median of a column in Postgres?"),
        ("assistant", "\nASSISTANT: Use percentile_cont(0.5) WITHIN GROUP (ORDER BY col) -- it's the standards-compliant way since Postgres 9.4."),
    ],
    query="how to compute median in postgres",
    target_position=1,
))

add(build_case(
    case_id="self-contained-airport-01",
    category="self-contained",
    turns_raw=[
        ("user", "Flight details: UA 1842, departs SFO 7:45am Friday, arrives EWR 4:20pm."),
        ("assistant", " Want me to add it to your calendar?"),
        ("user", " Yes and set a reminder 2 hours before departure."),
    ],
    query="what flight am I on Friday",
    target_position=0,
))

add(build_case(
    case_id="self-contained-medical-dose-01",
    category="self-contained",
    turns_raw=[
        ("user", "My kid is 40 pounds and has a fever of 102. Tylenol dose?"),
        ("assistant", " 10 mg/kg so about 180 mg every 4-6 hours. That's ~5.5 mL of children's liquid (160 mg/5 mL)."),
        ("user", " Thanks. Motrin safe to alternate?"),
        ("assistant", " Ibuprofen is 10 mg/kg every 6-8 hours if they're over 6 months. Alternating is fine but not needed if the fever responds."),
    ],
    query="what dose of tylenol for a 40 pound child with fever",
    target_position=1,
))

add(build_case(
    case_id="self-contained-travel-visa-01",
    category="self-contained",
    turns_raw=[
        ("user", "Do I need a visa to visit Japan as a US citizen for 10 days?"),
        ("assistant", " No, US citizens get 90 days visa-free for tourism. Bring a passport valid for the entire stay."),
    ],
    query="do I need a visa to visit japan as a us citizen",
    target_position=1,
))


# ─────────────────────────────────────────────────────────────────────────
# back-reference  (target turn references earlier content implicitly)
# ─────────────────────────────────────────────────────────────────────────

add(build_case(
    case_id="back-reference-kube-oomkill-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: I'm debugging a Kubernetes pod that keeps getting OOMKilled. It's the memory-heavy ML inference container we deployed last sprint."),
        ("assistant", "\nASSISTANT: What are the current memory limits and requests on the pod spec?"),
        ("user", "\nUSER: Limits are 4Gi, requests are 2Gi. The container is running Qwen3-0.6B for embeddings."),
        ("assistant", "\nASSISTANT: That model needs about 3.5Gi for inference with a reasonable batch size. Your limit is too close to its working set -- any spike in batch size or sequence length will tip it over."),
        ("user", "\nUSER: Ok I'll bump the limit to 8Gi. Thanks!"),
        ("assistant", "\nASSISTANT: Also consider setting GOMEMLIMIT or equivalent env vars so the runtime respects the cgroup limit before the kernel has to OOMKill."),
    ],
    query="what was causing the OOMKill",
    target_position=3,
    secondary_positions=[0],
    notes="Turn 3 diagnoses the cause in context; in isolation it barely mentions OOMKill.",
))

add(build_case(
    case_id="back-reference-sushi-chef-01",
    category="back-reference",
    turns_raw=[
        ("user", "I had an amazing omakase at Sushi Nakazawa last Thursday night."),
        ("user", " The chef was incredible, really warm and knowledgeable."),
        ("user", " I told my partner we should go back for our anniversary in June."),
    ],
    query="what was the chef like at the sushi place",
    target_position=1,
    secondary_positions=[0],
    notes="Turn 1 says 'the chef' without naming the sushi place.",
))

add(build_case(
    case_id="back-reference-pronoun-she-01",
    category="back-reference",
    turns_raw=[
        ("user", "My PM is Priya. She runs the storage team and owns the migration plan."),
        ("user", " She's on vacation next week so I'm covering standups."),
        ("user", " Can you help me draft a status update she can catch up on Monday?"),
    ],
    query="who is on vacation next week",
    target_position=1,
    secondary_positions=[0],
    notes="Turn 1 says 'she' referring to Priya from turn 0.",
))

add(build_case(
    case_id="back-reference-same-issue-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: The CI pipeline is failing on test_recall_respects_limit with 'embedding dim mismatch'."),
        ("assistant", "\nASSISTANT: Sounds like the test fixture inserts 768d vectors but the schema default is 1024d. Add configure_dimensions(768) in setup."),
        ("user", "\nUSER: Tried that, still failing."),
        ("assistant", "\nASSISTANT: Are you calling it before the inserts? Order matters; ALTER TABLE on a non-empty column errors out."),
        ("user", "\nUSER: Moved it. The same issue now appears in test_recall_enqueues_co_activation."),
    ],
    query="which test is now hitting the dim mismatch",
    target_position=4,
    notes="Turn 4 says 'the same issue' referring to the dim mismatch discussed.",
))

add(build_case(
    case_id="back-reference-that-error-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: gcloud auth login returns 'failed to verify token: invalid audience'."),
        ("assistant", "\nASSISTANT: Your application default credentials may be pointing at the wrong project."),
        ("user", "\nUSER: How do I fix it?"),
        ("assistant", "\nASSISTANT: Run 'gcloud auth application-default login' with the right --project. That error typically clears immediately."),
    ],
    query="how do I fix the invalid audience error",
    target_position=3,
    secondary_positions=[1],
    notes="Turn 3 says 'that error' -- only in context does it refer to the invalid audience.",
))

add(build_case(
    case_id="back-reference-the-decision-01",
    category="back-reference",
    turns_raw=[
        ("user", "We're choosing between Temporal and Airflow for orchestration."),
        ("assistant", " What are your requirements? Long-running workflows? Dynamic DAGs?"),
        ("user", " Mostly long-running state machines, some fan-out to 1000s of tasks."),
        ("assistant", " Temporal is the better fit then. Airflow's worker model strains at that fan-out and the retry semantics are weaker."),
        ("user", " Let's go with that decision. Can you help me scope the migration?"),
    ],
    query="which orchestrator did we choose",
    target_position=4,
    secondary_positions=[3],
    notes="Turn 4 says 'that decision' -- only meaningful in context.",
))

add(build_case(
    case_id="back-reference-going-with-02-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: I have three options for the embedding model: (1) OpenAI text-embedding-3-large, (2) Qwen3-Embedding-0.6B self-hosted, (3) BGE-large-en-v1.5."),
        ("assistant", "\nASSISTANT: For cost control and privacy, 2 is strongest. 1 is easiest but locks you into the vendor."),
        ("user", "\nUSER: What about retrieval quality?"),
        ("assistant", "\nASSISTANT: All three are competitive on English MTEB. Qwen3 is strong, BGE is a known quantity. OpenAI is marginally better but not decisive."),
        ("user", "\nUSER: Going with option 2 then."),
    ],
    query="which embedding model did we pick",
    target_position=4,
    secondary_positions=[0],
    notes="Turn 4 says 'option 2' without naming Qwen3.",
))

add(build_case(
    case_id="back-reference-anaphoric-yes-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: Should we enable connection pooling on the Postgres side with pgBouncer?"),
        ("assistant", "\nASSISTANT: For the workload you described (short-lived connections, many clients), yes, in transaction pooling mode."),
        ("user", "\nUSER: And keep the app-side pool small?"),
        ("assistant", "\nASSISTANT: Yes, cap it around 5-10 per instance. PgBouncer handles the fan-out."),
    ],
    query="should we use pgbouncer in transaction pooling mode",
    target_position=1,
    notes="Turn 1's 'yes, in transaction pooling mode' is contextual; in isolation it's a short agreement.",
))

add(build_case(
    case_id="back-reference-before-after-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: Our p99 latency was 450ms last week."),
        ("assistant", "\nASSISTANT: What changed?"),
        ("user", "\nUSER: We added a feature that fans out to three downstream services and aggregates."),
        ("assistant", "\nASSISTANT: That's your culprit. p99 is the sum of the slowest path through all three."),
        ("user", "\nUSER: It's now up to 1.2s, three times as bad."),
    ],
    query="how much has p99 latency degraded this week",
    target_position=4,
    secondary_positions=[0],
    notes="Turn 4 says 'three times as bad' without repeating the baseline number.",
))

add(build_case(
    case_id="back-reference-same-place-01",
    category="back-reference",
    turns_raw=[
        ("user", "Book me a trip to Banff for the first weekend of October."),
        ("assistant", " Cabin by Lake Louise, any preferences on budget?"),
        ("user", " Something under $400/night. Kitchenette preferred."),
        ("assistant", " Booked: Chalet Chateau Lake Louise, 2 nights, $385/night, check-in Friday Oct 3."),
        ("user", " Remember that for our anniversary next year -- book the same place."),
    ],
    query="what place is he asking to book again next year",
    target_position=4,
    secondary_positions=[3],
    notes="Turn 4 says 'the same place' without naming the hotel.",
))

add(build_case(
    case_id="back-reference-it-was-resolved-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: Remember that SSL handshake timeout we had last Tuesday on the replica?"),
        ("assistant", "\nASSISTANT: The one between pg_recall and the primary?"),
        ("user", "\nUSER: Yes. We pinned it to an MTU mismatch on the tailscale interface."),
        ("assistant", "\nASSISTANT: That explains the intermittent nature."),
        ("user", "\nUSER: It was resolved by setting mss-clamp on the replica."),
    ],
    query="how was the ssl handshake timeout fixed",
    target_position=4,
    secondary_positions=[2],
    notes="Turn 4 says 'it was resolved'; the 'it' references the timeout from turn 0.",
))

add(build_case(
    case_id="back-reference-my-earlier-answer-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: Is React Server Components production-ready?"),
        ("assistant", "\nASSISTANT: Yes, the Next.js App Router implementation has been stable for over a year."),
        ("user", "\nUSER: I've heard there are hydration issues on Safari."),
        ("assistant", "\nASSISTANT: My earlier answer was too absolute. There are known issues with Safari 15 and older; for 16+ it's stable. The Next.js team ships regular patches."),
    ],
    query="what caveat did you add about Safari",
    target_position=3,
    secondary_positions=[2],
    notes="Turn 3 says 'my earlier answer' -- a back-reference to the assistant's own turn 1.",
))

add(build_case(
    case_id="back-reference-opposite-direction-01",
    category="back-reference",
    turns_raw=[
        ("user", "The US-to-EU data pipeline is lagging by 15 minutes."),
        ("assistant", " Is that the ingestion side or the replication side?"),
        ("user", " Replication -- our Debezium connector is backed up."),
        ("assistant", " Check connector heap; the default 1G isn't enough once the binlog position falls behind."),
        ("user", " Good catch. What about the opposite direction?"),
    ],
    query="what about the EU-to-US direction",
    target_position=4,
    notes="Turn 4 says 'the opposite direction' -- back-reference to US-to-EU in turn 0.",
))

add(build_case(
    case_id="back-reference-she-again-01",
    category="back-reference",
    turns_raw=[
        ("user", "USER: I had lunch with Dr. Evans today about the grant."),
        ("assistant", "\nASSISTANT: How did it go?"),
        ("user", "\nUSER: Great -- she wants to co-PI and push the deadline back two weeks."),
        ("assistant", "\nASSISTANT: Moving the deadline is smart; co-PI means more work on acknowledgments. Did she commit?"),
        ("user", "\nUSER: Formally, yes. She'll send the letter tomorrow."),
    ],
    query="when will Dr Evans send the commitment letter",
    target_position=4,
    secondary_positions=[2],
    notes="Turn 4 pronouns + temporal reference; isolated encoding has no 'Dr Evans' token in this turn.",
))


# ─────────────────────────────────────────────────────────────────────────
# forward-reference  (query matches context established LATER)
# ─────────────────────────────────────────────────────────────────────────
# Qwen3-Embedding is a causal decoder. Late chunking SHOULDN'T help here;
# if anything it may slightly hurt. These are sanity checks.

add(build_case(
    case_id="forward-reference-explain-later-01",
    category="forward-reference",
    turns_raw=[
        ("user", "USER: I'll explain the root cause of the outage in a second."),
        ("assistant", "\nASSISTANT: Take your time."),
        ("user", "\nUSER: The backup cron overlapped with the WAL archive job and saturated disk throughput."),
    ],
    query="what was the root cause of the outage",
    target_position=2,
    notes="Turn 2 contains the explanation; turn 0 just promises it. Causal attention: turn 0's embedding does NOT benefit from turn 2's content.",
))

add(build_case(
    case_id="forward-reference-tell-you-01",
    category="forward-reference",
    turns_raw=[
        ("user", "USER: I have a question about the compile error."),
        ("assistant", "\nASSISTANT: Shoot."),
        ("user", "\nUSER: Why does 'cannot borrow as mutable' fire on a simple Vec push?"),
        ("assistant", "\nASSISTANT: Almost always a borrow split issue -- you're holding a reference from earlier that's still live."),
    ],
    query="what was the question about the compile error",
    target_position=2,
    notes="Turn 2 is the actual question; turn 0 just introduces it.",
))

add(build_case(
    case_id="forward-reference-heres-plan-01",
    category="forward-reference",
    turns_raw=[
        ("user", "Here's the plan."),
        ("user", " Monday we freeze the main branch. Tuesday we cut release-2.7. Wednesday we deploy to canary. Thursday we watch metrics. Friday we roll out 100%."),
        ("assistant", " What's the rollback plan?"),
    ],
    query="what are we doing on wednesday",
    target_position=1,
    notes="Turn 1 has the plan details; turn 0 is just the header.",
))

add(build_case(
    case_id="forward-reference-decision-01",
    category="forward-reference",
    turns_raw=[
        ("user", "USER: I'll tell you what I decided about the embedding model."),
        ("assistant", "\nASSISTANT: Curious to hear."),
        ("user", "\nUSER: Going with Qwen3-Embedding-0.6B for the privacy and cost story."),
    ],
    query="which embedding model was chosen",
    target_position=2,
    notes="Turn 2 reveals the decision; turn 0 just announces one is coming.",
))

add(build_case(
    case_id="forward-reference-there-is-catch-01",
    category="forward-reference",
    turns_raw=[
        ("user", "USER: Good news -- the perf regression is fixed."),
        ("assistant", "\nASSISTANT: Nice. Is there a catch?"),
        ("user", "\nUSER: Yes, we had to disable prefix caching. That'll hurt cold-start cache ratios but fixes the p99 spike."),
    ],
    query="what is the downside of the perf fix",
    target_position=2,
    notes="Turn 2 has the downside; turn 1 just asks if there's one.",
))

add(build_case(
    case_id="forward-reference-reveal-01",
    category="forward-reference",
    turns_raw=[
        ("user", "I have a surprise for our anniversary."),
        ("user", " Actually I can't keep it. I booked a long weekend in Kyoto, flights Thursday."),
        ("assistant", " That's amazing!"),
    ],
    query="where are we going for the anniversary",
    target_position=1,
    notes="Turn 1 reveals the destination; turn 0 says there is one without specifics.",
))

add(build_case(
    case_id="forward-reference-question-coming-01",
    category="forward-reference",
    turns_raw=[
        ("user", "USER: Before we ship, one more question."),
        ("assistant", "\nASSISTANT: Go ahead."),
        ("user", "\nUSER: Do we have alerting set up on the new sub_mnemes table growth rate?"),
    ],
    query="what is the pre-ship question about alerting",
    target_position=2,
    notes="Turn 2 has the actual question; turn 0 just introduces it.",
))

add(build_case(
    case_id="forward-reference-clarify-later-01",
    category="forward-reference",
    turns_raw=[
        ("user", "USER: The incident doc is ready but I want to revise one section."),
        ("assistant", "\nASSISTANT: Which one?"),
        ("user", "\nUSER: The timeline -- I want to add the Slack thread starting at 02:14 UTC where we first noticed the spike."),
    ],
    query="which incident doc section is being revised",
    target_position=2,
    notes="Turn 2 names the section; turn 0 mentions 'one section' without specifying.",
))


# ─────────────────────────────────────────────────────────────────────────
# short-session (1-2 turns)
# ─────────────────────────────────────────────────────────────────────────

add(build_case(
    case_id="short-session-single-turn-01",
    category="short-session",
    turns_raw=[
        ("assistant", "The capital of France is Paris."),
    ],
    query="what is the capital of France",
    target_position=0,
    notes="Single-turn session; target must be 0.",
))

add(build_case(
    case_id="short-session-two-turn-01",
    category="short-session",
    turns_raw=[
        ("user", "USER: What's the boiling point of water at sea level?"),
        ("assistant", "\nASSISTANT: 100 degrees Celsius, or 212 Fahrenheit."),
    ],
    query="what temperature does water boil at",
    target_position=1,
))

add(build_case(
    case_id="short-session-single-turn-02",
    category="short-session",
    turns_raw=[
        ("user", "The office wifi password for guests is guest-2026. Please don't share outside the company."),
    ],
    query="what is the guest wifi password",
    target_position=0,
))

add(build_case(
    case_id="short-session-two-turn-02",
    category="short-session",
    turns_raw=[
        ("user", "USER: What's the recommended log level for production Postgres?"),
        ("assistant", "\nASSISTANT: log_min_messages='warning', log_min_error_statement='error', log_statement='ddl'. Avoid 'all' in production; the volume will crush disks."),
    ],
    query="recommended log_min_messages in production postgres",
    target_position=1,
))

add(build_case(
    case_id="short-session-single-turn-03",
    category="short-session",
    turns_raw=[
        ("assistant", "Your teammate Priya's work hours are 9am-5pm IST, so 11:30pm-7:30am Pacific. Schedule sync meetings for her morning / your late evening."),
    ],
    query="when does priya work",
    target_position=0,
))

add(build_case(
    case_id="short-session-two-turn-03",
    category="short-session",
    turns_raw=[
        ("user", "USER: Where's the Grafana dashboard for API latency?"),
        ("assistant", "\nASSISTANT: grafana.internal/d/api-latency -- panels for p50/p90/p99 per service, plus error rate overlay."),
    ],
    query="where is the api latency grafana dashboard",
    target_position=1,
))


# ─────────────────────────────────────────────────────────────────────────
# multi-topic (session covers distinct topics, query targets one)
# ─────────────────────────────────────────────────────────────────────────

add(build_case(
    case_id="multi-topic-k8s-vs-python-01",
    category="multi-topic",
    turns_raw=[
        ("user", "USER: I'm picking between httpx and requests for a new async-heavy service. Thoughts?"),
        ("assistant", "\nASSISTANT: httpx for new code -- first-class async and HTTP/2. requests stays for simple sync scripts."),
        ("user", "\nUSER: Makes sense. Separately, my K8s deployment keeps OOMKilling on the ML inference pod."),
        ("assistant", "\nASSISTANT: Check memory limits vs model working set. Qwen3-0.6B needs ~3.5Gi with headroom."),
    ],
    query="what's the memory requirement for the inference model",
    target_position=3,
    secondary_positions=[2],
))

add(build_case(
    case_id="multi-topic-food-and-travel-01",
    category="multi-topic",
    turns_raw=[
        ("user", "We need a good sushi place in Manhattan and hotel suggestions near Times Square."),
        ("assistant", " For sushi: Sushi Nakazawa (W 58th) or Sushi Noz (UES). For hotels: The Knickerbocker or The Manhattan at Times Square."),
        ("user", " What's the best day to fly in for the trip?"),
        ("assistant", " Tuesday or Wednesday give the best airfare and less weekend crowds. Avoid Fridays."),
    ],
    query="which hotels were recommended near times square",
    target_position=1,
    secondary_positions=[0],
))

add(build_case(
    case_id="multi-topic-code-and-travel-01",
    category="multi-topic",
    turns_raw=[
        ("user", "USER: Help me write a short python script to rename files in a directory, date-prefixed."),
        ("assistant", "\nASSISTANT: Use pathlib and datetime: iterate Path(dir).iterdir(), rename to f'{today}_{path.name}'. Guard against collisions."),
        ("user", "\nUSER: Thanks. Switching gears -- recommendations for a 5-day trip to Portugal in May?"),
        ("assistant", "\nASSISTANT: 2 days Lisbon, 2 days Porto, 1 day Sintra or Douro valley. Book the train between Lisbon and Porto in advance."),
    ],
    query="how do I structure a trip to portugal for five days",
    target_position=3,
    secondary_positions=[2],
))

add(build_case(
    case_id="multi-topic-ml-vs-finance-01",
    category="multi-topic",
    turns_raw=[
        ("user", "USER: Is it worth fine-tuning Qwen3-0.6B for our domain or stick with prompting?"),
        ("assistant", "\nASSISTANT: Start with prompting and instruction tuning; only fine-tune if you have >10K high-quality examples AND a measurable prompt failure mode."),
        ("user", "\nUSER: Unrelated -- should I max out my 401k or put extra into a taxable brokerage?"),
        ("assistant", "\nASSISTANT: Max the 401k up to the match, then HSA if available, then brokerage. The tax deferral compounds meaningfully over 20+ years."),
    ],
    query="should I fine tune the embedding model",
    target_position=1,
    secondary_positions=[0],
))

add(build_case(
    case_id="multi-topic-security-and-cooking-01",
    category="multi-topic",
    turns_raw=[
        ("user", "Two things: should we rotate the pg_ghola admin password monthly, and what's a good oil for high-heat searing?"),
        ("assistant", " Password rotation: NIST 800-63B deprecates periodic rotation unless you suspect compromise. Use long passphrases + MFA instead. For searing: avocado oil (smoke point ~520F) or refined peanut oil."),
        ("user", " And for olive oil?"),
        ("assistant", " Extra virgin smokes at 375F -- too low for searing. Refined olive oil is fine up to ~465F."),
    ],
    query="what's the smoke point of avocado oil",
    target_position=1,
))

add(build_case(
    case_id="multi-topic-dns-and-recipe-01",
    category="multi-topic",
    turns_raw=[
        ("user", "USER: Pi-hole is returning NXDOMAIN for some adtech subdomains that used to resolve. Is that the blocklist or DNS config?"),
        ("assistant", "\nASSISTANT: Check /etc/pihole/gravity.db for the blocklist source; many adtech domains are on the default StevenBlack list."),
        ("user", "\nUSER: Also, quick: ratio of salt to water for dry-brining a turkey?"),
        ("assistant", "\nASSISTANT: 1 tsp kosher salt per pound of bird, applied 24-48 hours before roasting, uncovered in the fridge."),
    ],
    query="dry brine salt ratio for turkey",
    target_position=3,
))

add(build_case(
    case_id="multi-topic-oncall-and-gift-01",
    category="multi-topic",
    turns_raw=[
        ("user", "USER: Two things bugging me: oncall rotation for next month and what to get my sister for her birthday."),
        ("assistant", "\nASSISTANT: Share the current oncall roster -- I can suggest rebalancing. And tell me about your sister."),
        ("user", "\nUSER: Oncall: me week 1, Vinay week 2, Priya week 3, then me again week 4. She's a ceramicist, turning 35."),
        ("assistant", "\nASSISTANT: Rotation looks fine but you're doubling up -- swap week 4 with Vinay. For your sister: handmade pigment set or a kiln thermometer are thoughtful ceramicist gifts."),
    ],
    query="what should I get my sister for her birthday",
    target_position=3,
    secondary_positions=[2],
))

add(build_case(
    case_id="multi-topic-deploy-and-plant-01",
    category="multi-topic",
    turns_raw=[
        ("user", "USER: Our last ArgoCD deploy wedged on the loki app. Also, what's wrong with my fiddle leaf fig?"),
        ("assistant", "\nASSISTANT: For ArgoCD: check the sync status; wedged often means a stuck finalizer. Run argocd app terminate-op loki. For the fig: photo helps, but typically yellowing = overwatering, brown tips = underwatering."),
        ("user", "\nUSER: Lower leaves are yellow and dropping."),
        ("assistant", "\nASSISTANT: Classic overwatering. Let soil dry an inch deep before the next watering and check the drainage hole is clear."),
    ],
    query="what should I do about the yellowing fig leaves",
    target_position=3,
    secondary_positions=[1],
))


# ─────────────────────────────────────────────────────────────────────────
# Write JSONL
# ─────────────────────────────────────────────────────────────────────────

def main() -> None:
    out_path = Path(__file__).parent / "eval_cases.jsonl"
    seen: Dict[str, int] = {}
    for c in CASES:
        if c["id"] in seen:
            raise ValueError(f"duplicate case id: {c['id']}")
        seen[c["id"]] = 1
    with out_path.open("w", encoding="utf-8") as f:
        for c in CASES:
            f.write(json.dumps(c, ensure_ascii=False) + "\n")
    by_cat: Dict[str, int] = {}
    for c in CASES:
        by_cat[c["category"]] = by_cat.get(c["category"], 0) + 1
    print(f"Wrote {len(CASES)} cases to {out_path}")
    for cat in sorted(by_cat):
        print(f"  {cat:<20} {by_cat[cat]}")


if __name__ == "__main__":
    main()
