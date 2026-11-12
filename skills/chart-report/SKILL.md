---
name: chart-report
description: Produce charts from data in the code sandbox and a written report that references each chart and states what it shows. Use when a task asks for charts, plots, graphs, visual summaries or a report with figures built from tabular data.
version: 1.1.0
trust: builtin
scope: []
tools:
  - mcp:code-sandbox/execute_code
  - mcp:filesystem/read_text_file
  - mcp:filesystem/write_file
---

# Chart report

A chart is evidence for a sentence in the report. Make each chart to answer one
question, and make the report say what each chart shows. Follow these steps.

## 1. Plan the figures

From the task, list the questions the report must answer and pick one chart per
question. Choose the form by the question:

- change over time: a line chart;
- comparing categories: a bar chart, sorted by value, horizontal when labels are long;
- a distribution: a histogram or a box plot;
- the relationship between two numbers: a scatter plot;
- parts of a whole: a stacked bar; use a pie only for two or three parts.

Three to six charts is usually enough. Do not chart what one sentence can say.

## 2. Prepare the data

Load and clean the data in the sandbox with `execute_code` (Python, pandas). Name
each data file in `execute_code`'s `inputs` (its path, or the `led_…` id of an
earlier result): it is mounted read-only in `/inputs` for that run, Python starts
there, and `INPUTS` maps each input to its path. Never paste file contents into the
code. Save any chart or table file you want kept to `/outputs`. Compute the exact numbers each chart plots and print them,
because the report will quote them. If the data needs real profiling first, do
that before charting.

## 3. Draw each chart

Use matplotlib in the sandbox, one figure per call, and render it so the sandbox
returns the image. For every chart:

- a title that states the finding, for example "Sales fell 12% in Q3", not "Sales";
- labelled axes with units, and a y-axis starting at zero for bars;
- a legend only when there is more than one series;
- readable tick labels, no 3D effects, and the same colour for the same series
  across charts.

Number the charts (Figure 1, Figure 2, ...) in the order the report uses them,
and note for yourself the printed numbers behind each one. If a chart fails to
render, fix the code; if it still fails, give the numbers as a table and say the
chart could not be produced.

## 4. Write the report

Structure:

1. **Summary**: the main answer in three to five sentences.
2. **Findings**: one subsection per figure. Refer to the figure by number,
   state what it shows with the exact numbers you printed, and give the takeaway.
   A reader who cannot see the image must still get the point from the text.
3. **Method and caveats**: the data source, rows used, cleaning steps, and limits.

Every number in the text must match a number the code printed. Re-check them
before you finish.

## 5. Deliver

If the task names an output file, write the report with `write_file` (Markdown
by default); the write may need operator approval. Image files stay where the
sandbox put them unless the task asks for a location; say where they are, or that
they exist only as sandbox outputs. If the write is denied or not available,
return the full report as your final answer and say it was not written to disk.
