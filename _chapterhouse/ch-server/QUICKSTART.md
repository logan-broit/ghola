# Chapterhouse Quick Start

Connect Claude Code to your team's memory system in under 2 minutes.

## Prerequisites

- Claude Code CLI installed
- Your Chapterhouse API key (format: `ch_k1_...`)

## Setup

Add Chapterhouse as an MCP server:

```bash
claude mcp add -s user -t http ch-memory https://ch.example.com/mcp/stateless \
  --header "Authorization: Bearer ch_k1_YOUR_API_KEY_HERE"
```

**Replace `ch_k1_YOUR_API_KEY_HERE` with your actual API key.**

The `-s user` flag installs Chapterhouse for your user account across all projects.

That's it. Claude Code can now remember things across sessions.

## Usage

Claude Code automatically uses Chapterhouse when appropriate. You don't need to do anything special.

**Examples of what Claude remembers:**

- Project architecture decisions
- Your coding preferences and patterns
- Solutions to problems you've solved before
- Infrastructure details and configurations

**Test it:**

1. Tell Claude Code: *"Remember that I prefer TypeScript over JavaScript for new projects"*
2. In a later session, ask: *"What language should I use for my new API?"*

Claude will recall your preference.

## Verify Connection

Check that Chapterhouse is configured:

```bash
claude mcp list
```

You should see `ch-memory` in the list.

## Troubleshooting

**"Connection refused" or "401 Unauthorized"**
- Verify your API key is correct
- Check that the URL is accessible from your network

**Claude isn't remembering things**
- Confirm Chapterhouse appears in `claude mcp list`
- Try explicitly asking Claude to remember something
- Check that your API key hasn't expired

## Remove Chapterhouse

```bash
claude mcp remove ch-memory
```

---

Need help? Contact your platform team.
