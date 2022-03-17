package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Give this program some arguments!  It needs the name of an html file with your data to munge.\n")
	}
	someErrors := false
	for _, arg := range os.Args[1:] {
		// Parse the file and munge it.
		columns, entries, err := munge(arg)
		if err != nil {
			someErrors = true
			fmt.Fprintf(os.Stderr, "%q: failed: %s\n", arg, err)
			continue
		}
		// Emit csv.
		emitCsv(os.Stdout, columns, entries)
		// Done!
		fmt.Fprintf(os.Stderr, "%q: munged successfully: copy the above to a file (or use shell redirection) to save it.\n", arg)
	}
	if someErrors {
		os.Exit(14)
	}
}

func munge(filename string) (columns []string, entries []map[string]string, err error) {
	// Quick sanity check on the file type.
	if !strings.HasSuffix(filename, ".html") {
		return nil, nil, fmt.Errorf("not munging file %q; this tool works with html files (a '.html' suffix) only", filename)
	}

	// Pop 'er open.
	bs, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open html file %q: %w", filename, err)
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bs))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open html file %q: %w", filename, err)
	}

	// Check for the most likely data collection error and warn about it specifically.
	if doc.Find("iframe#transaction-statement-iframe").Length() > 0 {
		return nil, nil, fmt.Errorf("wrong html -- it looks like you got the enclosing document.  Check the README again -- did you do extraction correctly?  You have to get the content from inside the iframe element.  (Sorry this is complicated.  I didn't write the website.)")
	}

	// All the relevant data is in tables with this class.
	//  A lot of irrelevant data is too, but we'll sort that out later.
	tablesSelection := doc.Find("table.sw-datatable")
	if tablesSelection.Length() < 1 {
		return nil, nil, fmt.Errorf("found no shareworks data tables -- are you sure this is the right html?")
	}

	// Pluck out tables that have a header row that contains the text "Release".
	//  The "Release" tables are the only ones that are useful.
	//  (Other tables contain summaries, but the summaries are... basically useless, and exclude all of the facts that are actually relevant.  Amazing.)
	tablesSelection = tablesSelection.FilterFunction(func(i int, sel *goquery.Selection) bool {
		headerText := sel.Find("th.newReportTitleStyle").First().Text()
		return strings.Contains(headerText, "Release")
	})
	if tablesSelection.Length() < 1 {
		return nil, nil, fmt.Errorf("none of the shareworks data tables had titles containing the word 'Release' -- are you sure this is the right html?  We expected the events to all have 'Release' in the title somewhere.")
	}

	// BUT WAIT!  THERE'S MORE!
	// Look for h2 tags.  These contain the info about which kind of good we're handling.
	//  This is super important if you have more than one kind of stock or token being reported.
	//  Note that this information is NOT the actual stock or good itself -- it's the distribution schedule name.
	//   You'll have to demux that information back onto the actual stock or good manually with information in your hands as a human -- the document **literally** does not contain this information, as far as I can tell.
	// We have to do this in *the same query* as getting the tables, so that they're interleaved in the correct order in our selection here --
	//  the h2 tags aren't parents of the data they describe, they're just *before* the data they describe.  Additional "whee" for parsing :))))
	//   Can you imagine how great it would be if these tables actually say which unit they're denominated in?  But they don't :D :D :D :D
	//  So, that tablesSelection var earlier is demoted to just being another sanitychecker, and we'll loop over this below, looking for both tables and h2 tags.
	//   And we'll be re-doing the filter for tables-that-are-actually-relevant below, too.  Agghsdfhwefhsdfh.
	tablesAndHeadersSelection := doc.Find("h2, table.sw-datatable")

	// Okay, it's almost time to start accumulating data.
	// I'm gonna kinda try to normalize this to columnar as we go;
	//  and I'm not hard-coding any column headings,
	//   so, first encounter with a data entry in the whole document determins the order in which it will appear as a column.
	// See the definition of `columns` and `entries` at the top, in the function's returns.

	// We also need one slot of memory to remember the text of the last h2 tag we saw,
	//  because that's the distribution schedule name, and will apply to several rows, which we're about to loop over.
	var distributionScheduleName string

	// Go over the whole melange.
	// The headers become one column; the tables that are relevant each become one row in our sanitized data.
	// Yeah, one table becomes one row.  Yeah.  Yeahhhhh.
	// This is why your accountant didn't want to work with this format.  Because it's insane.  This is not how data should be formatted.
	// Anyway, let's go:
	tablesAndHeadersSelection.Each(func(i int, sel *goquery.Selection) {
		// First: see if this is:
		//  - a heading (e.g. might indicate which distribution schedule the following tables are for),
		//  - or if it's a table that we care about (e.g. it describes a distribution event),
		//  - or if it's one of the other tables that's useless (see earlier comments).
		// If it's a heading, we'll handle that in this logic block;
		// if it's a useless table, we'll skip out;
		// if it's a relevant table, the majority of the logic will continue below.
		switch {
		case sel.Is("h2"):
			distributionScheduleName = strings.TrimPrefix(strings.TrimSpace(sel.Text()), "Summary of ")
			return
		case sel.Is("table.sw-datatable"):
			headerText := sel.Find("th.newReportTitleStyle").First().Text()
			if !strings.Contains(headerText, "Release") {
				return
			}
			// if it does contain that word, it's relevant: continue...
		default:
			panic("unreachable, earlier filter should not have matched this")
		}

		// Make some temporary memory to put this row's data in as we find it.
		row := map[string]string{}
		entries = append(entries, row)

		// Append the distributionScheduleName as a column.
		accumulate(&columns, row, "Distribution Schedule", distributionScheduleName)

		// Pick a title for the event.
		//  We'll use that same table header that we happened to already look at above to filter the tables in the first place.
		accumulate(&columns, row, "Event", strings.TrimSpace(sel.Find("th.newReportTitleStyle").First().Text()))

		// Some brain genius made a four-column layout: two columns of two paired columns.  KVKV.
		// So we get to suss that back out.  Neato.
		// They tend to read top-bottom and then top-bottom again, and I'm actually going to bother to parse that ordering.
		var col1, col2, col3, col4 []string
		sel.Find("tr").Each(func(i int, sel *goquery.Selection) {
			sel.Find("td.staticViewTableColumn1").Each(func(i int, sel *goquery.Selection) {
				if i%2 == 0 {
					col1 = append(col1, strings.TrimSpace(sel.Text()))
				} else {
					col3 = append(col3, strings.TrimSpace(sel.Text()))
				}
			})
			sel.Find("td.staticViewTableColumn2").Each(func(i int, sel *goquery.Selection) {
				if i%2 == 0 {
					col2 = append(col2, strings.TrimSpace(sel.Text()))
				} else {
					col4 = append(col4, strings.TrimSpace(sel.Text()))
				}
			})
		})
		for i := range col1 {
			accumulate(&columns, row, col1[i], col2[i])
		}
		for i := range col3 {
			accumulate(&columns, row, col3[i], col4[i])
		}

		// For shits and giggles, they also made another KV attachment section, in a slightly different format.
		//  This one is at least only KV.  And fortunately we can distinguish it by the classes of the table cells.
		// TODO I haven't parsed this yet.
		/*
			// Code below is Not quite it.  The data is actually in another table that's a sibling *down* from the curren table.
			// This is such an obscenely terrible way to parse this that *I give up*.  I'm done here.
			// Fortunately, in my report, all the data found in this area is auxillary and can be regenerated from the data found in the earlier mechanism.
			sel.Find("tr").Each(func(i int, sel *goquery.Selection) {
				var key string
				ugh, _ := sel.Html()
				sel.Find("td.newReportCellStyle").Each(func(i int, sel *goquery.Selection) {
					switch i {
					case 0:
						key = strings.TrimSpace(sel.Text())
					case 1:
						accumulate(&columns, row, key, strings.TrimSpace(sel.Text()))
					}
				})
			})
		*/
	})

	return columns, entries, nil
}

func accumulate(columnOrder *[]string, row map[string]string, key string, value string) {
	row[key] = value
	for _, col := range *columnOrder {
		if col == key {
			return
		}
	}
	*columnOrder = append(*columnOrder, key)
}

func emitCsv(wr io.Writer, columnOrder []string, entries []map[string]string) error {
	c := csv.NewWriter(wr)
	c.UseCRLF = true
	// Write the first row, which is column headers.
	if err := c.Write(columnOrder); err != nil {
		fmt.Errorf("error while emitting csv: %w", err)
	}
	// Write the rest.
	row := make([]string, len(columnOrder))
	for _, ent := range entries {
		row = row[0:0]
		for _, col := range columnOrder {
			row = append(row, ent[col])
		}
		if err := c.Write(row); err != nil {
			fmt.Errorf("error while emitting csv: %w", err)
		}
	}
	c.Flush()
	return c.Error()
}
