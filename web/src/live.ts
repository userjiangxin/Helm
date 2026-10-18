// One SSE stream for the whole board. Every database change invalidates cached queries;
// at single-user scale refetching is cheaper than keeping a client-side event model in sync.
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";

export function useLiveUpdates(): boolean {
  const qc = useQueryClient();
  const [connected, setConnected] = useState(false);
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const es = new EventSource("/api/stream"); // EventSource reconnects on its own
    es.onopen = () => setConnected(true);
    es.onerror = () => setConnected(false);
    es.addEventListener("change", () => {
      clearTimeout(timer);
      timer = setTimeout(() => void qc.invalidateQueries(), 150);
    });
    return () => { clearTimeout(timer); es.close(); };
  }, [qc]);
  return connected;
}
