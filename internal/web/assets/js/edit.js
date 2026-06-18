// edit.js: inline-save for the metadata edit form. Intercepts the submit and
// PUTs the form as JSON to /api/books/{slug}; the plain form post stays as the
// no-JS fallback. Shows whether the edit was embedded into the file.

(function () {
  const form = document.getElementById("edit-form");
  const status = document.getElementById("edit-status");
  if (!form) return;
  const slug = window.BOOK_SLUG;

  function say(msg, isError) {
    if (!status) return;
    status.textContent = msg;
    status.classList.toggle("error", !!isError);
  }

  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    const f = form.elements;
    const body = {
      title: f.title.value,
      sortTitle: f.sortTitle.value,
      authors: f.authors.value,
      series: f.series.value,
      seriesIndex: f.seriesIndex.value,
      language: f.language.value,
      publisher: f.publisher.value,
      published: f.published.value,
      description: f.description.value,
    };
    say("Saving…", false);

    fetch("/api/books/" + encodeURIComponent(slug), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
      .then(function (resp) {
        if (!resp.ok) {
          return resp.text().then(function (t) {
            throw new Error(t.trim() || "save failed (" + resp.status + ")");
          });
        }
        return resp.json();
      })
      .then(function (data) {
        // The catalog edit is saved; the file embed runs in the background as a
        // tracked job. Follow it over the import SSE stream so the bar fills.
        say("Saved. Embedding into the file…", false);
        if (data.jobId) {
          watchEmbed(data.jobId);
        } else {
          say("Saved.", false);
        }
      })
      .catch(function (err) {
        say(err.message || "Save failed.", true);
      });
  });

  // watchEmbed follows the background embed job on the import SSE stream and
  // reflects its progress/outcome in the status line + bar. EventSource
  // auto-reconnects; we close it once the job reaches a terminal state.
  function watchEmbed(jobId) {
    const bar = document.getElementById("embed-bar");
    if (bar) {
      bar.hidden = false;
      bar.removeAttribute("value"); // indeterminate until the first fraction
    }
    if (!window.EventSource) {
      say("Saved. Embedding in the background…", false);
      return;
    }
    const es = new EventSource("/api/imports/stream");
    es.onmessage = function (ev) {
      let job;
      try {
        job = JSON.parse(ev.data);
      } catch (e) {
        return;
      }
      if (!job || job.id !== jobId) return;
      if (job.state === "running" && bar) {
        if (job.progress > 0) {
          bar.value = job.progress;
        } else {
          bar.removeAttribute("value");
        }
        say("Embedding into the file… " + (job.detail || ""), false);
      } else if (job.state === "done") {
        if (bar) bar.hidden = true;
        say("Saved and embedded in the file.", false);
        es.close();
      } else if (job.state === "skipped") {
        if (bar) bar.hidden = true;
        say("Saved. Not embedded in file: " + (job.error || "unknown"), false);
        es.close();
      } else if (job.state === "failed") {
        if (bar) bar.hidden = true;
        say("Saved, but embedding failed: " + (job.error || "unknown"), true);
        es.close();
      }
    };
  }

  // Cover override: PUT the chosen image's raw bytes; refresh the preview on
  // success. The server validates that it decodes as an image.
  const coverInput = document.getElementById("cover-input");
  const coverStatus = document.getElementById("cover-status");
  const coverPreview = document.querySelector(".cover-preview");
  if (coverInput) {
    coverInput.addEventListener("change", function () {
      const file = coverInput.files && coverInput.files[0];
      if (!file) return;
      if (coverStatus) coverStatus.textContent = "Uploading cover…";
      fetch("/book/" + encodeURIComponent(slug) + "/cover", {
        method: "PUT",
        headers: { "Content-Type": file.type || "application/octet-stream" },
        body: file,
      })
        .then(function (resp) {
          if (!resp.ok) {
            return resp.text().then(function (t) {
              throw new Error(t.trim() || "cover upload failed (" + resp.status + ")");
            });
          }
          if (coverStatus) {
            coverStatus.textContent = "Cover updated.";
            coverStatus.classList.remove("error");
          }
          // Bust the cache so the new override shows.
          if (coverPreview) {
            coverPreview.style.display = "";
            coverPreview.src = "/book/" + encodeURIComponent(slug) + "/cover?t=" + Date.now();
          }
        })
        .catch(function (err) {
          if (coverStatus) {
            coverStatus.textContent = err.message || "Cover upload failed.";
            coverStatus.classList.add("error");
          }
        });
    });
  }
})();
