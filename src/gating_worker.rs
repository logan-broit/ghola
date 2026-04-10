// pg_ghola::gating_worker -- Async attribute extraction worker
//
// Drains the gating_queue table and extracts entities, dates, and intent
// from mneme content. Follows the same pattern as the contradiction worker.
// Populates the thalamic gating columns for Tier 2 deep gate filtering.

use pgrx::bgworkers::{BackgroundWorker, SignalWakeFlags};
use pgrx::prelude::*;
use std::time::{Duration, Instant};

use crate::PG_GHOLA_DATABASE;

// ---------------------------------------------------------------------------
// Gating worker state machine (same cadence as contradiction worker)
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum GatingWorkerState {
    Active,   // 5s poll
    Idle,     // 30s poll
    Dormant,  // 300s poll
}

impl GatingWorkerState {
    pub fn name(&self) -> &'static str {
        match self {
            Self::Active => "active",
            Self::Idle => "idle",
            Self::Dormant => "dormant",
        }
    }

    pub fn poll_interval_ms(&self) -> u64 {
        match self {
            Self::Active => 5_000,
            Self::Idle => 30_000,
            Self::Dormant => 300_000,
        }
    }
}

pub struct GatingStateMachine {
    pub state: GatingWorkerState,
    last_activity: Instant,
}

impl GatingStateMachine {
    pub fn new() -> Self {
        Self {
            state: GatingWorkerState::Active,
            last_activity: Instant::now(),
        }
    }

    pub fn transition(&mut self, items_processed: i64) {
        if items_processed > 0 {
            self.state = GatingWorkerState::Active;
            self.last_activity = Instant::now();
            return;
        }
        let idle_secs = self.last_activity.elapsed().as_secs();
        match self.state {
            GatingWorkerState::Active if idle_secs >= 30 => {
                self.state = GatingWorkerState::Idle;
            }
            GatingWorkerState::Idle if idle_secs >= 300 => {
                self.state = GatingWorkerState::Dormant;
            }
            _ => {}
        }
    }

    pub fn poll_interval(&self) -> Duration {
        Duration::from_millis(self.state.poll_interval_ms())
    }
}

// ---------------------------------------------------------------------------
// Extraction functions (heuristic, no LLM dependency)
// ---------------------------------------------------------------------------

/// Title prefixes that indicate the following word(s) are a name.
const TITLE_PREFIXES: &[&str] = &["dr.", "mr.", "mrs.", "ms.", "prof."];

/// Common sentence-starting words to ignore when they're capitalized.
const COMMON_STARTERS: &[&str] = &[
    "the", "a", "an", "this", "that", "these", "those", "my", "your", "his",
    "her", "its", "our", "their", "i", "we", "you", "he", "she", "it", "they",
    "is", "are", "was", "were", "will", "would", "could", "should", "can",
    "do", "does", "did", "has", "have", "had", "if", "when", "where", "what",
    "which", "who", "how", "why", "but", "and", "or", "so", "yet", "for",
    "nor", "not", "no", "all", "each", "every", "some", "any", "most",
    "also", "just", "then", "now", "here", "there", "very", "too", "much",
    "many", "more", "less", "only", "still", "already", "even", "never",
    "always", "often", "sometimes", "after", "before", "since", "while",
    "because", "although", "though", "however", "therefore", "thus",
    "meanwhile", "otherwise", "instead", "furthermore", "moreover",
    "with", "from", "into", "about", "over", "under", "between",
];

/// Extract entity-like terms from text.
///
/// Finds:
/// - Mid-sentence capitalized words (proper nouns not at sentence start)
/// - Title-prefixed names (Dr., Mr., Mrs., Ms., Prof.)
/// - Multi-word capitalized sequences (e.g. "New York", "Sarah Chen")
/// - CamelCase/tech terms (e.g. "PostgreSQL", "ArgoCD", "JavaScript")
/// - @mentions
/// - Quoted terms
pub fn extract_entities(text: &str) -> Vec<String> {
    let mut entities = Vec::new();

    // 1. Extract @mentions
    for word in text.split_whitespace() {
        let clean = word.trim_matches(|c: char| !c.is_alphanumeric() && c != '@');
        if clean.starts_with('@') && clean.len() > 1 {
            entities.push(clean.to_lowercase());
        }
    }

    // 2. Extract quoted terms
    for delim in ['"', '\u{201C}'] {
        let close = match delim {
            '\u{201C}' => '\u{201D}',
            _ => delim,
        };
        let mut chars = text.chars().peekable();
        while let Some(c) = chars.next() {
            if c == delim {
                let term: String = chars.by_ref().take_while(|&ch| ch != close).collect();
                let trimmed = term.trim();
                if !trimmed.is_empty() && trimmed.len() < 100 {
                    entities.push(trimmed.to_lowercase());
                }
            }
        }
    }

    // 3. Extract CamelCase/mixed-case tech terms (e.g. PostgreSQL, ArgoCD, JavaScript)
    for word in text.split_whitespace() {
        let clean = word.trim_matches(|c: char| !c.is_alphanumeric());
        if clean.len() >= 2 && is_camelcase(clean) {
            entities.push(clean.to_lowercase());
        }
    }

    // 4. Extract capitalized words and sequences (mid-sentence proper nouns)
    let sentences = split_sentences(text);
    for sentence in &sentences {
        let words: Vec<&str> = sentence.split_whitespace().collect();
        let mut i = 0;
        while i < words.len() {
            let clean = words[i].trim_matches(|c: char| !c.is_alphanumeric() && c != '.');
            let lower = clean.to_lowercase();

            // Check for title prefix (Dr., Mr., etc.)
            if TITLE_PREFIXES.contains(&lower.as_str()) {
                // Consume the title + following capitalized words
                let mut name_parts = vec![clean];
                let mut j = i + 1;
                while j < words.len() {
                    let next = words[j].trim_matches(|c: char| !c.is_alphanumeric());
                    if !next.is_empty() && next.chars().next().map_or(false, |c| c.is_uppercase()) {
                        name_parts.push(next);
                        j += 1;
                    } else {
                        break;
                    }
                }
                if name_parts.len() >= 2 {
                    entities.push(name_parts.join(" ").to_lowercase());
                }
                i = j;
                continue;
            }

            // Skip if first word in sentence (likely sentence-starting capitalization)
            let is_sentence_start = i == 0;

            if !clean.is_empty() && clean.chars().next().map_or(false, |c| c.is_uppercase()) {
                // Multi-word capitalized sequence
                let mut seq = vec![clean];
                let mut j = i + 1;
                while j < words.len() {
                    let next = words[j].trim_matches(|c: char| !c.is_alphanumeric());
                    if !next.is_empty() && next.chars().next().map_or(false, |c| c.is_uppercase()) {
                        seq.push(next);
                        j += 1;
                    } else {
                        break;
                    }
                }

                if seq.len() >= 2 {
                    // Multi-word sequences are almost always entities
                    entities.push(seq.join(" ").to_lowercase());
                    i = j;
                    continue;
                } else if !is_sentence_start {
                    // Single capitalized word mid-sentence = proper noun
                    if !COMMON_STARTERS.contains(&lower.as_str()) {
                        entities.push(lower);
                    }
                }
            }

            i += 1;
        }
    }

    entities.sort();
    entities.dedup();
    entities
}

/// Check if a word is CamelCase or mixed-case (has uppercase letters after the first char).
fn is_camelcase(word: &str) -> bool {
    let chars: Vec<char> = word.chars().collect();
    if chars.len() < 2 {
        return false;
    }
    // Must have at least one lowercase and one uppercase after position 0
    let has_lower = chars.iter().skip(1).any(|c| c.is_lowercase());
    let has_upper = chars.iter().skip(1).any(|c| c.is_uppercase());
    has_lower && has_upper
}

/// Known abbreviations that shouldn't trigger sentence splits.
const ABBREVIATIONS: &[&str] = &[
    "dr.", "mr.", "mrs.", "ms.", "prof.", "jr.", "sr.", "st.", "vs.", "etc.",
    "inc.", "ltd.", "co.", "corp.", "dept.", "univ.", "gen.", "gov.",
];

/// Split text into sentences using period + space or newline boundaries.
/// Avoids splitting on abbreviation periods (Dr., Mr., etc.).
fn split_sentences(text: &str) -> Vec<String> {
    let mut sentences = Vec::new();
    let mut current = String::new();

    let chars: Vec<char> = text.chars().collect();
    let mut i = 0;
    while i < chars.len() {
        current.push(chars[i]);

        let is_period = chars[i] == '.' || chars[i] == '!' || chars[i] == '?';
        let followed_by_space = i + 1 < chars.len()
            && (chars[i + 1] == ' ' || chars[i + 1] == '\n');

        if chars[i] == '\n' {
            let trimmed = current.trim().to_string();
            if !trimmed.is_empty() {
                sentences.push(trimmed);
            }
            current.clear();
        } else if is_period && followed_by_space && chars[i] == '.' {
            // Check if this period is part of an abbreviation
            let current_trimmed = current.trim();
            let last_word = current_trimmed
                .rsplit_once(|c: char| c.is_whitespace())
                .map(|(_, w)| w)
                .unwrap_or(current_trimmed)
                .to_lowercase();

            if !ABBREVIATIONS.contains(&last_word.as_str()) {
                let trimmed = current.trim().to_string();
                if !trimmed.is_empty() {
                    sentences.push(trimmed);
                }
                current.clear();
            }
        } else if is_period && followed_by_space && chars[i] != '.' {
            // ! or ? always end sentences
            let trimmed = current.trim().to_string();
            if !trimmed.is_empty() {
                sentences.push(trimmed);
            }
            current.clear();
        }

        i += 1;
    }
    let trimmed = current.trim().to_string();
    if !trimmed.is_empty() {
        sentences.push(trimmed);
    }
    sentences
}

/// Extract ISO date patterns from text.
/// Returns date strings (not parsed timestamps -- parsing happens at UPDATE time).
pub fn extract_dates(text: &str) -> Vec<String> {
    let mut dates = Vec::new();

    // ISO dates: YYYY-MM-DD
    let mut i = 0;
    let bytes = text.as_bytes();
    while i + 9 < bytes.len() {
        // Look for pattern: 4 digits, dash, 2 digits, dash, 2 digits
        if bytes[i].is_ascii_digit()
            && bytes[i + 1].is_ascii_digit()
            && bytes[i + 2].is_ascii_digit()
            && bytes[i + 3].is_ascii_digit()
            && bytes[i + 4] == b'-'
            && bytes[i + 5].is_ascii_digit()
            && bytes[i + 6].is_ascii_digit()
            && bytes[i + 7] == b'-'
            && bytes[i + 8].is_ascii_digit()
            && bytes[i + 9].is_ascii_digit()
        {
            // Check word boundary (not preceded or followed by alphanumeric)
            let before_ok = i == 0 || !bytes[i - 1].is_ascii_alphanumeric();
            let after_ok = i + 10 >= bytes.len() || !bytes[i + 10].is_ascii_alphanumeric();
            if before_ok && after_ok {
                let date_str = &text[i..i + 10];
                dates.push(date_str.to_string());
            }
            i += 10;
        } else {
            i += 1;
        }
    }

    dates
}

/// Classify intent from text content.
/// Returns "fact" as default when no stronger signal is found.
pub fn classify_intent(text: &str) -> Option<String> {
    let lower = text.to_lowercase();
    let checks: &[(&str, &[&str])] = &[
        ("decision", &["decided", "chose", "picked", "switched to", "went with", "settled on"]),
        ("preference", &["prefer", "like to", "enjoy", "favorite", "rather", "love to"]),
        ("plan", &["plan to", "going to", "will ", "schedule", "intend", "aim to"]),
        ("question", &["?", "how do", "what is", "can you", "wondering", "how does"]),
        ("experience", &["went to", "visited", "tried", "attended", "saw ", "met "]),
    ];

    let mut best: Option<(&str, usize)> = None;
    for (intent, keywords) in checks {
        let count = keywords.iter().filter(|kw| lower.contains(**kw)).count();
        if count > 0 && (best.is_none() || count > best.unwrap().1) {
            best = Some((intent, count));
        }
    }

    Some(best.map(|(intent, _)| intent.to_string())
        .unwrap_or_else(|| "fact".to_string()))
}

// ---------------------------------------------------------------------------
// Queue processing
// ---------------------------------------------------------------------------

/// Process one item from the gating queue.
/// Returns 1 if an item was processed, 0 if queue was empty.
fn process_one_gating_item() -> i64 {
    let result = Spi::get_two::<pgrx::Uuid, pgrx::Uuid>(
        "WITH d AS ( \
             DELETE FROM ghola.gating_queue \
             WHERE id = (SELECT id FROM ghola.gating_queue ORDER BY id LIMIT 1) \
             RETURNING mneme_id, workspace_id \
         ) SELECT mneme_id, workspace_id FROM d"
    );

    match result {
        Ok((Some(mneme_id), Some(_ws_id))) => {
            let content = Spi::get_one::<String>(&format!(
                "SELECT content FROM ghola.mnemes WHERE id = '{mneme_id}'"
            ));

            if let Ok(Some(text)) = content {
                let entities = extract_entities(&text);
                let dates = extract_dates(&text);
                let intent = classify_intent(&text);

                let mut sets = Vec::new();
                if !entities.is_empty() {
                    let arr = entities.iter()
                        .map(|e| format!("'{}'", e.replace('\'', "''")))
                        .collect::<Vec<_>>().join(",");
                    sets.push(format!("entities = ARRAY[{arr}]::text[]"));
                }
                if !dates.is_empty() {
                    let arr = dates.iter()
                        .map(|d| format!("'{d}'::timestamptz"))
                        .collect::<Vec<_>>().join(",");
                    sets.push(format!("content_dates = ARRAY[{arr}]::timestamptz[]"));
                }
                if let Some(ref i) = intent {
                    sets.push(format!("intent = '{i}'"));
                }

                if !sets.is_empty() {
                    Spi::run(&format!(
                        "UPDATE ghola.mnemes SET {} WHERE id = '{mneme_id}'",
                        sets.join(", ")
                    )).unwrap_or_else(|e| log!("gating worker: update failed: {e}"));
                }
            }
            1
        }
        _ => 0,
    }
}

// ---------------------------------------------------------------------------
// Stats updates
// ---------------------------------------------------------------------------

fn write_gating_stats(state: &str, items: i64, poll_ms: i32) {
    let queue_depth = Spi::get_one::<i64>(
        "SELECT count(*) FROM ghola.gating_queue",
    )
    .unwrap_or(Some(0))
    .unwrap_or(0);

    Spi::run(&format!(
        "UPDATE ghola.gating_worker_stats SET \
             state = '{state}', \
             queue_depth = {queue_depth}, \
             items_processed = {items}, \
             last_process_at = now(), \
             poll_interval_ms = {poll_ms}, \
             updated_at = now() \
         WHERE id = 1",
    ))
    .unwrap_or_else(|e| log!("pg_ghola gating worker: stats update failed: {e}"));
}

// ---------------------------------------------------------------------------
// Background worker entry point
// ---------------------------------------------------------------------------

#[pg_guard]
#[no_mangle]
pub extern "C-unwind" fn gating_worker_main(_arg: pg_sys::Datum) {
    BackgroundWorker::attach_signal_handlers(SignalWakeFlags::SIGHUP | SignalWakeFlags::SIGTERM);

    let db_name = PG_GHOLA_DATABASE
        .get()
        .and_then(|cs| cs.to_str().ok().map(|s| s.to_string()))
        .unwrap_or_else(|| "memories".to_string());

    BackgroundWorker::connect_worker_to_spi(Some(&db_name), None);

    log!(
        "pg_ghola gating worker: started, connected to database '{db_name}'"
    );

    let mut sm = GatingStateMachine::new();
    let mut total_items: i64 = 0;

    // Mark worker as running
    BackgroundWorker::transaction(|| {
        Spi::run(
            "UPDATE ghola.gating_worker_stats SET \
                 state = 'active', \
                 started_at = now(), \
                 updated_at = now() \
             WHERE id = 1",
        )
        .unwrap_or_else(|e| log!("pg_ghola gating worker: init failed: {e}"));
    });

    loop {
        if BackgroundWorker::sigterm_received() {
            log!("pg_ghola gating worker: SIGTERM received, shutting down");
            let state_name = sm.state.name().to_string();
            let poll_ms = sm.state.poll_interval_ms() as i32;
            let items = total_items;
            BackgroundWorker::transaction(move || {
                write_gating_stats(&state_name, items, poll_ms);
            });
            log!(
                "pg_ghola gating worker: shutdown complete, {} items processed",
                total_items
            );
            break;
        }

        let processed = BackgroundWorker::transaction(|| {
            process_one_gating_item()
        });

        if processed > 0 {
            total_items += 1;
        }
        sm.transition(processed);

        let state_name = sm.state.name().to_string();
        let poll_ms = sm.state.poll_interval_ms() as i32;
        let items = total_items;
        BackgroundWorker::transaction(move || {
            write_gating_stats(&state_name, items, poll_ms);
        });

        BackgroundWorker::wait_latch(Some(sm.poll_interval()));
    }
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    // ── State machine tests ──

    #[test]
    fn test_gating_state_machine_starts_active() {
        let sm = GatingStateMachine::new();
        assert_eq!(sm.state, GatingWorkerState::Active);
    }

    #[test]
    fn test_gating_active_to_idle() {
        let mut sm = GatingStateMachine::new();
        sm.last_activity = Instant::now() - Duration::from_secs(31);
        sm.transition(0);
        assert_eq!(sm.state, GatingWorkerState::Idle);
    }

    #[test]
    fn test_gating_idle_to_dormant() {
        let mut sm = GatingStateMachine::new();
        sm.state = GatingWorkerState::Idle;
        sm.last_activity = Instant::now() - Duration::from_secs(301);
        sm.transition(0);
        assert_eq!(sm.state, GatingWorkerState::Dormant);
    }

    #[test]
    fn test_gating_dormant_to_active() {
        let mut sm = GatingStateMachine::new();
        sm.state = GatingWorkerState::Dormant;
        sm.last_activity = Instant::now() - Duration::from_secs(600);
        sm.transition(1);
        assert_eq!(sm.state, GatingWorkerState::Active);
    }

    #[test]
    fn test_gating_poll_intervals() {
        assert_eq!(GatingWorkerState::Active.poll_interval_ms(), 5_000);
        assert_eq!(GatingWorkerState::Idle.poll_interval_ms(), 30_000);
        assert_eq!(GatingWorkerState::Dormant.poll_interval_ms(), 300_000);
    }

    // ── Entity extraction tests ──

    #[test]
    fn test_extract_entities_multi_word_names() {
        let entities = extract_entities("I met Sarah Chen at the conference.");
        assert!(entities.contains(&"sarah chen".to_string()),
            "should extract multi-word name 'Sarah Chen', got: {:?}", entities);
    }

    #[test]
    fn test_extract_entities_mid_sentence_single_cap() {
        let entities = extract_entities("I talked to Sarah about the project.");
        assert!(entities.contains(&"sarah".to_string()),
            "should extract mid-sentence proper noun 'Sarah', got: {:?}", entities);
    }

    #[test]
    fn test_extract_entities_skips_sentence_start() {
        let entities = extract_entities("The cat sat on the mat.");
        assert!(!entities.contains(&"the".to_string()),
            "should NOT extract sentence-starting 'The', got: {:?}", entities);
    }

    #[test]
    fn test_extract_entities_title_prefix() {
        let entities = extract_entities("I saw Dr. Smith at the clinic.");
        assert!(entities.contains(&"dr. smith".to_string()),
            "should extract title-prefixed name 'Dr. Smith', got: {:?}", entities);
    }

    #[test]
    fn test_extract_entities_at_mentions() {
        let entities = extract_entities("Talked to @loganb about deployment.");
        assert!(entities.contains(&"@loganb".to_string()),
            "should extract @mention, got: {:?}", entities);
    }

    #[test]
    fn test_extract_entities_quoted_terms() {
        let entities = extract_entities(r#"The "thalamic gating" feature is ready."#);
        assert!(entities.contains(&"thalamic gating".to_string()),
            "should extract quoted term, got: {:?}", entities);
    }

    #[test]
    fn test_extract_entities_camelcase() {
        let entities = extract_entities("We use PostgreSQL and ArgoCD for infrastructure.");
        assert!(entities.contains(&"postgresql".to_string()),
            "should extract CamelCase term 'PostgreSQL', got: {:?}", entities);
        assert!(entities.contains(&"argocd".to_string()),
            "should extract CamelCase term 'ArgoCD', got: {:?}", entities);
    }

    #[test]
    fn test_extract_entities_deduplicates() {
        let entities = extract_entities("Sarah met Sarah at the Sarah convention.");
        let sarah_count = entities.iter().filter(|e| *e == "sarah").count();
        assert_eq!(sarah_count, 1, "should deduplicate entities, got: {:?}", entities);
    }

    #[test]
    fn test_extract_entities_empty_text() {
        let entities = extract_entities("");
        assert!(entities.is_empty(), "empty text should produce no entities");
    }

    #[test]
    fn test_extract_entities_no_caps() {
        let entities = extract_entities("all lowercase words here.");
        assert!(entities.is_empty(),
            "all-lowercase text should produce no entities, got: {:?}", entities);
    }

    // ── Date extraction tests ──

    #[test]
    fn test_extract_dates_iso() {
        let dates = extract_dates("The meeting is on 2026-04-09.");
        assert!(dates.contains(&"2026-04-09".to_string()),
            "should extract ISO date, got: {:?}", dates);
    }

    #[test]
    fn test_extract_dates_multiple() {
        let dates = extract_dates("Between 2026-01-15 and 2026-03-20.");
        assert_eq!(dates.len(), 2, "should extract both dates, got: {:?}", dates);
    }

    #[test]
    fn test_extract_dates_none() {
        let dates = extract_dates("No dates in this text.");
        assert!(dates.is_empty(), "should return empty for no dates");
    }

    #[test]
    fn test_extract_dates_embedded_in_text() {
        let dates = extract_dates("Created on 2026-04-09 and updated.");
        assert_eq!(dates.len(), 1);
        assert_eq!(dates[0], "2026-04-09");
    }

    // ── Intent classification tests ──

    #[test]
    fn test_classify_intent_decision() {
        assert_eq!(classify_intent("I decided to use Rust for the project."),
            Some("decision".to_string()));
    }

    #[test]
    fn test_classify_intent_preference() {
        assert_eq!(classify_intent("I prefer dark mode for coding."),
            Some("preference".to_string()));
    }

    #[test]
    fn test_classify_intent_question() {
        assert_eq!(classify_intent("How do I configure the database?"),
            Some("question".to_string()));
    }

    #[test]
    fn test_classify_intent_plan() {
        assert_eq!(classify_intent("I plan to deploy the service next week."),
            Some("plan".to_string()));
    }

    #[test]
    fn test_classify_intent_experience() {
        assert_eq!(classify_intent("I visited the datacenter yesterday."),
            Some("experience".to_string()));
    }

    #[test]
    fn test_classify_intent_fact_default() {
        assert_eq!(classify_intent("The server runs on port 8080."),
            Some("fact".to_string()));
    }

    #[test]
    fn test_classify_intent_empty() {
        // Empty text should still return fact as default
        assert_eq!(classify_intent(""), Some("fact".to_string()));
    }
}
