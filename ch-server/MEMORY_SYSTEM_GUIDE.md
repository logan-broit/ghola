# Chapterhouse Memory System Guide

This document explains how the Chapterhouse MCP (Model Context Protocol) memory system works, how memories are stored and retrieved, and best practices for managing them.

---

## How Memory Retrieval Works

### Memories are not Automatically Loaded

Memories are opt-in, not automatic. They only appear in conversation context when explicitly retrieved.

When you send a message to Claude:
1. Your message is sent to Claude
2. Claude analyzes your question
3. If Claude decides memories would help, it calls the `recall` tool
4. The recall tool searches the memory database and returns relevant results (typically 5-10 memories max)
5. Only those returned memories are added to the conversation context
6. Claude answers using your message and the recalled memories

### Token Usage Implications

- Stored memories do not consume tokens - Having 1000 memories stored does not increase token usage
- Only recalled memories consume tokens - When Claude retrieves 5 memories, only those 5 are added to context
- No recalls means no memory tokens - If Claude does not call recall for a conversation, zero memory tokens are used
- Chapterhouse MCP connection does not leak memories - Simply having the server connected does not inject memories into context

### The Trade-off

Benefit: Token-efficient - memories only loaded when needed

Drawback: Claude might not always recall the right memories (or any memories) when it should

---

## Memory Types

Memories can be classified as `factual` (standards, policies), `experiential` (solutions, lessons learned), or `working` (session context, auto-expires after 7 days). Defaults to `factual` if not specified. Filter with `list_memories(memory_type="experiential")`.

---

## Tagging Strategy

### Project-Specific vs. Generic Knowledge

Chapterhouse-specific memories should use only the `"ch"` tag:
```
["ch"]
```

Examples:
- "Chapterhouse is deployed in ch-system namespace"
- "Chapterhouse uses two repos: ch-server (backend) and ch-web (frontend)"
- "Chapterhouse backend is Go with PostgreSQL and Qdrant"

Generic infrastructure/standards should use descriptive tags without project names:
```
["docker", "kubernetes", "security"]
["helm", "registry"]
```

Examples:
- "Docker builds must use --platform linux/amd64"
- "CA bundle must be embedded for MITM proxy"
- "Helm charts must include security contexts"

### Why This Matters

When working on a different project (not Chapterhouse), Claude can recall only generic infrastructure memories:
```
recall(query="how to build docker image", tags=["docker"])
```

This avoids polluting other project contexts with Chapterhouse-specific deployment details.

### Tag Filtering Behavior

The `list_memories(tags=[...])` function works as an AND filter - it returns only memories that have all specified tags.

Example:
- `tags=["docker"]` - Returns all memories tagged with "docker"
- `tags=["docker", "ch"]` - Returns only memories tagged with both "docker" and "ch"

---

## Available Memory Operations

### What You Can Do (via natural language)

**List all memories**
   "List all memories" → Claude calls `list_memories()`

**List memories by tag or type**
   "List memories tagged with 'ch'" → Claude calls `list_memories(tags=["ch"])`
   "List experiential memories" → Claude calls `list_memories(memory_type="experiential")`

**Delete specific memories**
   "Forget memory 5" → Claude calls `forget(fact_id=5)`

**Recall memories with context**
   "What do you know about Chapterhouse?" → Claude calls `recall(query="Chapterhouse", tags=["ch"])`

### What You Cannot Do (yet)

**Direct command syntax** - No `/recall --tags docker` or GUI available

**Browse tags independently** - Cannot see available tags without asking Claude

**Force specific recall behavior** - Cannot control which tags Claude uses during automatic recalls

### How to Interact with Memories

Since there is no direct UI or command syntax, you interact with memories through natural language requests to Claude:

- "List all memories tagged with 'kubernetes'"
- "What Docker standards do we have?"
- "Forget memory 12"
- "Show me everything about Chapterhouse deployment"

Claude interprets your request and calls the appropriate memory tool.

---

## Creating Memories with Natural Language

You don't need explicit commands like "remember this" or "store in memory." Claude proactively creates memories when it learns important information during conversation.

**Examples:**
- Stating a fact: "Our container registry is at registry.example.com" → Claude stores it
- Solving a problem: "Fixed that by adding CORS headers" → Claude remembers the solution
- Explicit request: "Remember that shadcn charts wraps recharts" → Also works

**What Claude should decide automatically:**
- Whether to create the memory (based on importance)
- Memory type: `factual` (standards/policies), `experiential` (lessons learned), or `working` (temporary session context)
- Tags: Inferred from content (e.g., ["registry"] or ["nginx", "debugging"])

**Memory Type Classification:**
- `factual`: Infrastructure details, standards, configurations, architectural decisions
- `experiential`: Solutions to problems, debugging insights, lessons learned from mistakes
- `working`: Temporary session context, auto-expires after 7 days

**Current behavior:** Claude should infer the appropriate memory type but may default to `factual` if not explicitly considered. Users can be explicit: "store as experiential memory" or "remember this as working memory for this session."

---

## Memory Management Best Practices

### When to Create Memories

Create memories for:
- Infrastructure standards that apply across projects (Docker, Kubernetes, Helm)
- Corporate environment specifics (registry URLs, kubeconfig locations, CA bundles)
- Project-specific architecture decisions
- Operational procedures that need to be remembered across sessions
- Solutions to problems that took time to debug

Do not create memories for:
- Temporary or frequently changing information
- Information already well-documented in code comments
- Implementation details better suited for code itself
- Conversation-specific context that will not be needed later

### Keeping Memories Useful

1. Be specific and self-contained - Each memory should be understandable without additional context
2. Tag appropriately - Use project-specific tags for project details, generic tags for reusable knowledge
3. Regular cleanup - Periodically review and delete outdated memories
4. Avoid duplication - Before creating a new memory, check if similar information already exists

---

## Example Tagging Scenarios

### Scenario 1: Docker Build Standard (Generic)

```
Fact: "All Docker builds for Kubernetes clusters must use
       --platform linux/amd64 for Apple Silicon compatibility"
Tags: ["docker", "kubernetes", "platform", "apple-silicon", "build-standards"]
```

Why these tags: Generic infrastructure knowledge applicable to any project.

### Scenario 2: Chapterhouse Deployment (Project-Specific)

```
Fact: "Chapterhouse system deploys to ch-system namespace with both
       API and web frontend components"
Tags: ["ch"]
```

Why only "ch": Project-specific detail that should not pollute other project contexts.

### Scenario 3: Security Standard (Generic)

```
Fact: "Helm charts must include securityContext with runAsNonRoot: true,
       runAsUser: 1000, and drop ALL capabilities"
Tags: ["helm", "kubernetes", "security", "securitycontext", "best-practices"]
```

Why these tags: Reusable security standard for all Kubernetes deployments.

---

## Technical Architecture

### Memory Storage

Memories are stored in the Chapterhouse PostgreSQL database with:
- Semantic embeddings for similarity search
- Tag-based indexing for filtering
- Full-text content with timestamps

### Recall Mechanism

When Claude calls `recall(query="...", tags=[...])`:
1. Query is converted to an embedding vector
2. Semantic search finds similar memories using vector similarity
3. Optional tag filter is applied (AND operation)
4. Top N results (default 10) are returned
5. Results are injected into conversation context

### MCP Server Connection

The Chapterhouse MCP server provides tools:
- `remember(fact, tags)` - Store a new memory
- `recall(query, tags, limit)` - Search and retrieve memories
- `forget(fact_id)` - Delete a memory by ID
- `list_memories(tags, limit)` - List all memories, optionally filtered

These tools are available to Claude Code whenever the Chapterhouse MCP server is connected.

---

## Troubleshooting

### "Claude is not recalling memories I know exist"

- Claude may not have judged them relevant to your query
- Try being more explicit: "Search your memories for X"
- Check if the memory exists: "List all memories tagged with X"
- The recall query might not match the memory content semantically

### "I want to prevent memories from appearing in certain contexts"

- Use project-specific tags (like "ch") consistently
- When working on other projects, ask Claude to only recall generic infrastructure memories
- Example: "Help me with Docker builds, but only recall generic Docker knowledge, not Chapterhouse-specific stuff"

### "Too many memories are being recalled"

- Ask Claude to be more selective: "Only recall memories directly related to X"
- The default recall limit is 10 - this keeps token usage reasonable

---

*Generated with Claude Code*
