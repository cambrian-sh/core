---
name: exploratory-analysis
description: Explore a dataset in the code sandbox, from loading and profiling to cleaning, statistics and caveated findings. Use for CSV, Excel, JSON, Parquet or SQLite data when the task asks what the data shows, for trends, distributions, outliers, comparisons or data quality.
version: 1.1.0
trust: builtin
scope: []
tools:
  - mcp:code-sandbox/execute_code
  - mcp:filesystem/list_directory
  - mcp:filesystem/get_file_info
  - mcp:filesystem/read_text_file
  - mcp:sqlite/list_tables
  - mcp:sqlite/execute_sql
---

# Exploratory analysis

Every number you report must come from code you ran in the sandbox, not from
mental arithmetic or a guess. Follow these steps in order.

## 1. Frame the question

Write down what the task wants to learn from the data, the unit of analysis
(one row is a what?), and any filters or time window it names. If the task only
says "analyse this", the questions are: what is in it, how clean is it, and what
are the three most notable patterns.

## 2. Load the data

- **Files.** Locate the file (`list_directory`, `get_file_info`), then name it in
  `execute_code`'s `inputs`: its path as the file tools show it, or the ledger id
  (`led_…`) of an earlier result. The kernel reads each input with your access and
  mounts it read-only in `/inputs` for that run only; Python starts there, so open
  it by its file name (`pd.read_csv("bank-export.csv")`), or through the `INPUTS`
  dict, which maps each input as you wrote it to its path. Never retype or paste
  file contents into the code. If an input is refused, report that instead of
  analysing something else. Write any file you produce to `/outputs`; the kernel
  records it and returns its ledger id to cite.
- **SQLite.** List tables with `list_tables`, then read with `SELECT` queries in
  `execute_sql`. Only read: never run `INSERT`, `UPDATE`, `DELETE`, `DROP` or
  `ALTER` unless the task explicitly asks you to change the database.
- In the sandbox, use pandas (`execute_code`, language `python`). Keep each code
  call short and print what you need to see; large printouts waste the context.

## 3. Profile the schema

Print and record: the row and column counts, each column's type, the count and
share of missing values, the number of distinct values, and five sample rows.
Check the types are what they should be (dates parsed as dates, numbers not
stored as text) and that a key column is actually unique.

## 4. Clean, and log every change

Fix only what the analysis needs: parse types, normalise obvious spelling
variants, drop exact duplicates, and decide how to treat missing values and
impossible values (negative ages, dates in the future). Keep a list of every
cleaning step with the number of rows it touched. Never silently drop rows;
report how many were removed and why.

## 5. Compute statistics

Start broad, then answer the question:

- per numeric column: count, mean, median, standard deviation, min, max and
  quartiles; per categorical column: the top values and their shares;
- the relationships the question is about: group-by summaries, crosstabs,
  correlations, changes over time;
- outliers, with the rule you used to call them outliers.

Use the sample size in every comparison. A difference between two small groups
may be noise; say so, and do not claim causation from a correlation.

## 6. Report findings

Return:

1. **Answer**: two to four sentences that answer the question.
2. **Findings**: each one a claim with its number, the code-computed statistic
   behind it, and the column(s) it comes from.
3. **Data quality and cleaning**: the profile in brief and your cleaning log.
4. **Caveats**: sampling, missing data, assumptions, and what the data cannot
   tell you.

If a step failed in the sandbox, say which step and why instead of filling the
gap with an estimate.
