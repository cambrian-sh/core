---
name: organise-files
description: Organise files and folders safely by creating folders and moving or renaming files, never by rewriting them. Use for filing, sorting, tidying, renaming by date or title, and moving documents such as receipts, invoices, photos or downloads into folders.
version: 1.0.0
trust: builtin
scope: []
tools:
  - mcp:filesystem/list_directory
  - mcp:filesystem/directory_tree
  - mcp:filesystem/search_files
  - mcp:filesystem/get_file_info
  - mcp:filesystem/read_text_file
  - mcp:pdf-reader/read
  - mcp:filesystem/create_directory
  - mcp:filesystem/move_file
---

# Organise files

Organising moves and renames files. It never changes what is inside them. Follow
these steps in order.

## 1. Pin down the request

- **Which files.** The folder to look in and the rule that selects files ("PDFs
  from last month", "every photo from 2025"). If the task names no folder, use
  the working directory you were given. Never touch files outside the request.
- **Where they go.** The destination folder.
- **New names.** The naming pattern, if any ("YYYY-MM-DD Title.pdf"). Keep the
  original extension.

## 2. Inventory

1. List the source folder (`list_directory`; `get_file_info` for dates and sizes).
2. Select the files the rule matches. When the rule depends on content (a date
   printed on a receipt), read each candidate (`read_text_file`, or `read` for a
   PDF) and note the value you found and where.
3. Leave a file out, and say so, when you cannot tell whether it matches. Do not
   guess a date or a title.

## 3. Plan every move before making any

Write the plan as a list: `old path -> new path`, one line per file.

- Two files must never get the same new name. Add a short suffix (" (2)") to the
  second rather than overwrite the first.
- A target that already exists is a conflict: leave that file where it is and
  report it.
- Characters a file name cannot hold (`/ \ : * ? " < > |`) are replaced with `-`.

## 4. Do it

1. Create the destination folder if it is missing (`create_directory`).
2. Move each file with `move_file`. A move to a new name is the rename: one call
   per file, from the old path to the new path.
3. Never copy a file by writing its content, and never "move" a file by writing a
   new one: writing a document's text, a summary or placeholder content over or
   in place of a file destroys it. If `move_file` is not among your tools, stop
   and say that the move needs it (or hand the step back with `yield_subgoal`).
4. A refused or denied move is not retried with another tool. Report it.

## 5. Check and report

1. List the destination folder and confirm each planned file is there under its
   new name, and gone from the source.
2. Report: every move made (`old -> new`), every file left out and why, and every
   conflict or refusal.
