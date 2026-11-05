---
name: summarise
description: Write a faithful summary of a given source, with no invented content and a refusal when there is no source. Use for summaries, abstracts, digests, TL;DRs and key-point lists of documents, files, pages, transcripts or earlier step results.
version: 1.0.0
trust: builtin
scope: []
tools:
  - mcp:filesystem/read_text_file
  - mcp:filesystem/read_file
  - mcp:filesystem/read_multiple_files
  - mcp:pdf-reader/read
  - mcp:markitdown/convert_to_markdown
---

# Summarise

A summary is a smaller copy of what the source says. It is never a place for
your own knowledge, guesses or opinions. Follow these steps in order.

## 1. Find the source

Work out exactly what you are summarising before writing a word:

- **Text in the task or the workspace.** Text pasted into the task, or a prior
  step's result in the workspace, is the source. If the workspace shows a `cid`,
  that pointer is the source; resolve it rather than summarising the preview.
- **A file path.** Read it: `read_text_file` for text, `read_multiple_files` for
  several small files, `pdf-reader` `read` for a PDF, and
  `convert_to_markdown` for office documents and HTML.
- **A URL.** Summarise it only if you can actually fetch it with a tool you hold.

**If there is no source, stop.** Say plainly that you were not given anything to
summarise and ask for the text, file or link. Do not write a summary of a topic
"from general knowledge", and do not summarise a source you could not open:
report the error instead.

If the source is longer than you can read in one go, read it in parts and keep
short notes per part. Say in the summary which parts you read if you could not
read all of it.

## 2. Pin down the shape

Use what the task asks for. If it says nothing, default to:

- one opening sentence that states what the source is and its main point;
- three to seven bullets, most important first;
- a length of about a tenth of the source, and never longer than the source.

Honour any explicit length, audience or format (one paragraph, 50 words,
"for a non-specialist", a table) exactly.

## 3. Draft from your notes

- Keep the source's own claims, numbers, names and dates. Copy figures exactly,
  with their units.
- Keep the source's stance. If it is uncertain ("may", "early results"), the
  summary is uncertain too. Do not turn a hedge into a fact.
- Attribute opinions: "the author argues", "the report recommends".
- Keep the proportions. A point the source spends one line on does not become
  the headline.
- Leave out what the source does not say: no background, no evaluation, no
  recommendations of your own unless the task asks for them, and then label
  them clearly as yours, separate from the summary.

## 4. Check it against the source

Read the draft sentence by sentence and confirm each one is in the source.
Check every number and name against your notes. Delete any sentence you cannot
trace. If the source contradicts itself, say so instead of picking one side.

## 5. Answer

Return the summary as your final answer. When the source was one or more files
or pages, end with a `Sources:` line naming each one. If part of the source was
unreadable or skipped, say which part in one line at the end.
