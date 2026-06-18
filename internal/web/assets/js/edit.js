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
        if (data.embedded) {
          say("Saved and embedded in the file.", false);
        } else {
          say("Saved. Not embedded in file: " + (data.embedReason || "unknown"), false);
        }
      })
      .catch(function (err) {
        say(err.message || "Save failed.", true);
      });
  });

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
