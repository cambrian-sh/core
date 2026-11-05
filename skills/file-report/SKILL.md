---
name: file-report
description: Produce a structured written report from files in a working directory, checking every claim against its source. Use for summaries, reviews, audits, briefings and status reports built from documents, notes, data or code.
version: 1.0.0
trust: builtin
scope: []
tools:
  - mcp:filesystem/list_directory
  - mcp:filesystem/directory_tree
  - mcp:filesystem/search_files
  - mcp:filesystem/read_text_file
  - mcp:filesystem/read_file
  - mcp:filesystem/read_multiple_files
  - mcp:filesystem/write_file
---

# File report

Follow these steps in order. Do not skip the verification step. A report that
says something its sources do not say is worse than no report.

## 1. Pin down the request

Before reading anything, write down for yourself:

- **Question.** What must the report answer? Restate it in one sentence.
- **Reader.** Who will read it, and what will they do with it? A manager deciding
  something needs a recommendation first; an auditor needs evidence first.
- **Scope.** Which directory or files are in scope. If the task names none, use
  the working directory you were given. Never read outside it.
- **Output.** The file path and format to write (default: `report.md` in the
  working directory, Markdown). If the task says to return the report instead of
  writing it, or no write tool is available to you, the report is your final answer.

If the question is ambiguous in a way that changes what you would read, say which
reading you chose in the report's first section rather than guessing silently.

## 2. Gather

1. List the scope first (`list_directory` or `directory_tree`). Note how many files
   there are and of which kinds. Use `search_files` to find files by name when the
   tree is large.
2. Decide what to read. Read the files that bear on the question; skip build
   output, lock files, binaries and caches. If there are more relevant files than
   you can read, say so and state which ones you read.
3. Read each chosen file (`read_text_file`, or `read_multiple_files` for several
   small ones). While reading, keep short notes of the form
   `file -> fact (line or section)`. Copy numbers, dates and names exactly.
4. Note contradictions between files as you find them. They belong in the report.

## 3. Outline

Write the section list before any prose. The default shape is:

1. **Summary**: three to five sentences that answer the question.
2. **Findings**: one subsection per finding, most important first. Each finding
   states the claim, then the evidence, then the source file(s).
3. **Gaps and contradictions**: what the files do not settle, and where they disagree.
4. **Sources**: every file the report relies on, one per line.

Adapt the shape to the request (a status report may want "Done / In progress /
Blocked"), but keep a summary first and the sources last.

## 4. Draft

- Lead with the answer. Put background after it, not before.
- One claim per sentence where you can. Give a number with its unit and its source.
- Name every source by its path relative to the working directory, for example
  `notes/2026-q3.md`.
- Separate what the files say from what you infer. Mark inferences with
  "This suggests" or a similar phrase.
- Do not pad. If a section has nothing to say, write one line saying so.

## 5. Verify every claim against its source

Go through the draft claim by claim. For each one:

- Re-open the cited file and check the claim is there and says what you wrote.
  Check numbers digit by digit.
- If a claim has no source, either find one or delete the claim.
- If you misquoted, fix the sentence, not the source citation.

Then check the summary: every sentence in it must be backed by a finding below it.

## 6. Write the file

Write the finished report with `write_file` to the output path from step 1. The
write may need operator approval. If it is denied or no write tool is available,
return the full report as your final answer and say that it was not written to disk.
Never overwrite an existing file the task did not name. Pick a new name instead
and say which one you used.

Finish with a short final answer: the path written (or "not written"), a one-line
summary, and the number of source files used.
