package alerts

import (
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/MKuranowski/WarsawGTFS/realtime/gtfs"
	"github.com/MKuranowski/WarsawGTFS/realtime/util"
	"github.com/cenkalti/backoff/v4"
)

// Options represents available options for generating alerts
type Options struct {
	GtfsRtTarget    string
	JSONTarget      string
	HumanReadable   bool
	ThrowLinkErrors bool
}

// exclusiveHttpClient is a pair of *html.Client and *sync.Mutex
// to avoid spamming a single host with requests
type exclusiveHTTPClient struct {
	m *sync.Mutex
	c *http.Client
}

func (client exclusiveHTTPClient) Get(url string) (resp *http.Response, err error) {
	client.m.Lock()
	defer client.m.Unlock()
	return client.c.Get(url)
}

// allRssItems fetches urlImpediments and urlChanges and retrieves all
// RssItems that should be converted into Alerts
func allRssItems(client exclusiveHTTPClient) (items []*rssItem, err error) {
	// Load both RSS feeds
	impedimentsRss, err := getRss(client, urlImpediments, "REDUCED_SERVICE")
	if err != nil {
		return
	}

	changesRss, err := getRss(client, urlChanges, "OTHER_EFFECT")
	if err != nil {
		return
	}

	// Make a slice for all RssItems
	items = make([]*rssItem, 0, len(changesRss.Channel.Items)+len(impedimentsRss.Channel.Items))
	for _, impedimentItem := range impedimentsRss.Channel.Items {
		items = append(items, impedimentItem)
	}
	for _, changeItem := range changesRss.Channel.Items {
		items = append(items, changeItem)
	}

	return
}

// Make auto-magically creates GTFS-Realtime feeds with alert data
func Make(client *http.Client, routeMap map[string]sort.StringSlice, opts Options) (err error) {
	// Create a container for all Alerts
	var container AlertContainer
	container.Timestamp = time.Now()
	container.Time = container.Timestamp.Format("2006-01-02 15:04:05")

	// Wrap the http.Client exclusiveHTTPClient to avoid spamming wtp.waw.pl
	exclusiveClient := exclusiveHTTPClient{
		m: &sync.Mutex{},
		c: client,
	}

	// Load both RSS feeds
	log.Println("Fetching RSS feeds")
	items, err := allRssItems(exclusiveClient)
	if err != nil {
		return
	}

	// Convert RSS items to Alert objects
	log.Println("Casting RSS items to Alert objects")
	for _, item := range items {
		var a *Alert
		a, err = alertFromRssItem(item)
		if err != nil {
			return
		}

		container.Alerts = append(container.Alerts, a)
	}

	// Load data from alert links
	err = container.LoadExternal(exclusiveClient, routeMap, opts.ThrowLinkErrors)
	if err != nil {
		return
	}

	// Filter invalid alerts
	container.Filter()

	// Export to a JSON file
	if opts.JSONTarget != "" {
		log.Println("Exporting to JSON")
		container.SaveJSON(opts.JSONTarget)
	}

	// Export to a GTFS-Realtime file
	if opts.GtfsRtTarget != "" {
		log.Println("Exporting to GTFS-Realtime")
		container.SavePB(opts.GtfsRtTarget, opts.HumanReadable)
	}
	return
}

// routesResource is a pair of resource pointing to a GTFS file and a routeMap
type routesResource struct {
	Resource util.Resource
	RouteMap map[string]sort.StringSlice
}

// Update automatically updates the RouteMap if the Resource has changed
func (rr *routesResource) Update() error {
	// Check for GTFS updates
	shouldUpdate, err := rr.Resource.Check()
	if err != nil {
		return err
	} else if shouldUpdate || rr.RouteMap == nil {
		log.Println("GTFS has changed, updating avaiilable route_ids.")

		var newData io.ReadCloser
		var gtfsObj *gtfs.Gtfs

		// Try to fetch updated GTFS
		newData, err := rr.Resource.Fetch()
		if err != nil {
			return err
		}

		// Load the new GTFS
		defer newData.Close()
		gtfsObj, err = gtfs.NewGtfsFromReader(newData)
		if err != nil {
			return err
		}
		defer gtfsObj.Close()

		// Load GTFS routers
		rr.RouteMap, err = gtfs.ListGtfsRoutes(gtfsObj)
		if err != nil {
			return err
		}
	}
	return nil
}

// Loop automatically updates the GTFS-RT alerts file
func Loop(client *http.Client, gtfsResource util.Resource, sleepTime time.Duration, opts Options) (err error) {
	// We don't use ticker as there's no guarantee that a single pass
	// will be shorter then sleepTime.
	// And, it doesn't really matter, it's not mission crticial that the alerts feed is updated
	// every `sleepTime`, it's fine if it's updated sleepTime + a few seconds.
	rr := &routesResource{Resource: gtfsResource}
	exponantialBackoff := backoff.NewExponentialBackOff()
	exponantialBackoff.Multiplier = 2
	loopBackoff := backoff.WithMaxRetries(exponantialBackoff, 12)

	for {
		// Try to update the underlaying GTFS data
		err = rr.Update()
		if err != nil {
			return
		}

		// Try updating the GTFS-RT
		loopBackoff.Reset()
		for sleep := time.Duration(0); sleep != backoff.Stop; sleep = loopBackoff.NextBackOff() {
			// Print error when backing off
			if sleep != 0 {
				// Log when backingoff
				sleepUntil := time.Now().Add(sleep).Format("15:04:05")
				log.Printf(
					"Updating the GTFS-RT Alerts failed. Backoff until %s. Error: %q.\n",
					sleepUntil, err.Error(),
				)

				// Sleep for the backoff
				time.Sleep(sleep)
			}

			// Try to update the GTFS-RT
			err = Make(client, rr.RouteMap, opts)

			// If no errors were encountered, break out of the backoff loop
			if err == nil {
				log.Println("GTFS-RT Alerts updated successfully.")
				break
			}
		}
		if err != nil {
			return
		}

		// Sleep until next try
		time.Sleep(sleepTime)
	}
}