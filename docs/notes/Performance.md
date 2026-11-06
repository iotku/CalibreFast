There are various potential bottlenecks for performance given faster hardware.

- Disk access speeds / concurrency (e.g. SSD vs HDD)
  --> Testing is done on a USB 3.0 external HDD (slightly above average HDD speeds), with caching to a reasonably fast internal SSD.
  ImageSym in covers.go is set to 8 but may have benefit in being increased on faster disks/flash storage, but needs investigation to see if it is worth it.
  -- we should find good ways to profile this, it may not be anywhere near our bottleneck even on an HDD

- Slow network access (e.g. 3G vs 5G)
  --> Most testing is synthetic through dev tools network throttling, but some testing is done on a 4G LTE connection.
  -- We try to aggressively stop loading images if they are not in the current viewport, but this is not always possible due to the way the browser handles image loading and often we end up overloading images which can result in contention with loading new pages when moving through the library quickly. Under http 1.1 (which is the main testing area) we believe the tcp concurrent connections are limited to about 6 per domain, so we may be overloading the browser with too many images to load at once. This is a potential area for improvement if we could use multiple domains to load images from.
  -- We should also investgate a good pipeline for using http 2.0 or 3.0 to improve image loading and reduce contention with other requests while maintaining good performance on more limited http 1.1 connections.

- Caching
  --> The front-end should cache aggressively, but the back-end should also cache results where possible. The back-end is currently caching cover image paths, uuids, and modification times in memory and image thumbnails on disk. At this point it may be more sensible to just use the database directly (while I think there may be issues with contention or read/write locks, it might actually be more performant than the filesystem). The browser should cache thumbnails effectively, but the main images are cached only temporarily as they will take a considerable amount of space.
  -- Currently Authors and tags are not cached, but this is a potential area for improvement. The back-end should cache these results in memory or on disk to avoid repeated database queries.
  --> If we do not start using the sqliteDB directly for everything, we may be able to pre-cache the results of the authors and tags queries in memory or on disk to avoid repeated database queries. This would be a good area for improvement, especially if we can cache the results of the authors and tags queries in memory or on disk to avoid repeated database queries.
  -- Also, the sqliteDB should be assumed to be on slow storage which may go to sleep (my external hard drive goes into standby mode which takes upwards of 15s to recover from). If possible it may be good to load the entire database into memory and work off of that, then sync the database over time back to disk.
  -- Ideally we only query the database once to avoid issues with read/write locks to leave space for very specific search queries and metadata edits without blocking otherwise.
