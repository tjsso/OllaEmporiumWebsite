const menuButton = document.querySelector('.menu-toggle');
const navigation = document.querySelector('#main-navigation');

if (menuButton && navigation) {
  const closeMenu = () => {
    menuButton.setAttribute('aria-expanded', 'false');
    navigation.classList.remove('is-open');
  };

  menuButton.addEventListener('click', () => {
    const isOpen = menuButton.getAttribute('aria-expanded') === 'true';
    menuButton.setAttribute('aria-expanded', String(!isOpen));
    navigation.classList.toggle('is-open', !isOpen);
  });

  navigation.addEventListener('click', (event) => {
    if (event.target.closest('a')) closeMenu();
  });

  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && menuButton.getAttribute('aria-expanded') === 'true') {
      closeMenu();
      menuButton.focus();
    }
  });
}

function safeImageURL(value) {
  if (typeof value !== 'string' || /[\\\u0000-\u001f\u007f]/.test(value)) return '';
  if (value.startsWith('/') && !value.startsWith('//')) return value;
  try {
    const url = new URL(value);
    return url.protocol === 'https:' && !url.username && !url.password ? url.href : '';
  } catch (_) {
    return '';
  }
}

function renderListing(listing, source) {
  if (typeof listing.id !== 'string' || !listing.id || typeof listing.title !== 'string') {
    throw new Error('Invalid listing');
  }
  const item = document.createElement('li');
  item.className = 'featured-item';
  const link = document.createElement('a');
  const params = new URLSearchParams({ source, id: listing.id });
  link.href = '/product?' + params.toString();

  const imageFrame = document.createElement('div');
  imageFrame.className = 'featured-image';
  const showPlaceholder = () => {
    const placeholder = document.createElement('span');
    placeholder.className = 'featured-image-placeholder';
    placeholder.textContent = 'Image unavailable';
    imageFrame.replaceChildren(placeholder);
  };
  const src = safeImageURL(listing.image_url);
  if (src) {
    const image = document.createElement('img');
    image.src = src;
    image.alt = '';
    image.loading = 'lazy';
    image.addEventListener('error', showPlaceholder, { once: true });
    imageFrame.append(image);
  } else {
    showPlaceholder();
  }

  const details = document.createElement('div');
  details.className = 'featured-card-details';
  if (listing.price) {
    const price = document.createElement('p');
    price.className = 'featured-price';
    price.textContent = listing.price;
    details.append(price);
  }
  const title = document.createElement('h3');
  title.textContent = listing.title;
  details.append(title);
  link.append(imageFrame, details);
  item.append(link);
  return item;
}

function setupCarousel(section, track, status, endpoint, source) {
  if (!section || !track || !status) return Promise.resolve(null);
  const controls = section.querySelector('.featured-controls');
  const previous = controls.querySelector('.featured-previous');
  const next = controls.querySelector('.featured-next');

  const updateControls = () => {
    const first = track.firstElementChild;
    const last = track.lastElementChild;
    if (!first || !last) {
      controls.hidden = true;
      return;
    }
    const viewport = track.getBoundingClientRect();
    const canGoBack = first.getBoundingClientRect().left < viewport.left - 1;
    const canGoForward = last.getBoundingClientRect().right > viewport.right + 1;
    controls.hidden = !canGoBack && !canGoForward;
    previous.disabled = !canGoBack;
    next.disabled = !canGoForward;
  };
  const scrollListings = (direction) => {
    const card = track.firstElementChild;
    if (!card) return;
    const gap = parseFloat(getComputedStyle(track).columnGap) || 0;
    track.scrollBy({ left: direction * (card.getBoundingClientRect().width + gap) });
  };
  previous.addEventListener('click', () => scrollListings(-1));
  next.addEventListener('click', () => scrollListings(1));
  track.addEventListener('scroll', updateControls, { passive: true });
  window.addEventListener('resize', updateControls);

  status.textContent = 'Loading pieces…';
  const dataPromise = fetch(endpoint)
    .then((response) => {
      if (!response.ok) throw new Error('Listings unavailable');
      return response.json();
    })
    .then((data) => {
      const listings = data && (source === 'etsy' ? data.listings : data.products);
      if (!Array.isArray(listings) || (source === 'etsy' && !['mock', 'etsy'].includes(data.source))) {
        throw new Error('Invalid listings response');
      }
      return data;
    });

  dataPromise
    .then((data) => {
      const listings = source === 'etsy' ? data.listings : data.products;
      if (!listings.length) {
        status.textContent = source === 'etsy'
          ? 'There are no active Etsy listings right now. Visit the Etsy shop to see more.'
          : 'Website-only pieces will appear here when available.';
        return;
      }
      const items = listings.map((listing) => renderListing(listing, source));
      track.replaceChildren(...items);
      track.hidden = false;
      status.textContent = source === 'etsy' && data.source === 'mock'
        ? 'Preview only — these are example pieces, not live Etsy listings.'
        : source === 'etsy' ? 'Explore all active listings from the Etsy shop.' : 'Discover pieces available only on this website.';
      if (source === 'site') document.querySelector('#etsy-featured').before(section);
      updateControls();
    })
    .catch(() => {
      status.textContent = source === 'etsy'
        ? 'Etsy listings are unavailable right now. Visit the Etsy shop to see the latest pieces.'
        : 'Website-only pieces are unavailable right now. Please try again later.';
    });
  return dataPromise.catch(() => null);
}

function setupEditorialImages(etsyData, siteData) {
  const products = [];
  const seen = new Set();
  const addProducts = (listings, source) => {
    if (!Array.isArray(listings)) return;
    for (const listing of listings) {
      if (!listing || typeof listing.id !== 'string' || !listing.id.trim() ||
          typeof listing.title !== 'string' || !listing.title.trim()) continue;
      const imageURL = safeImageURL(listing.image_url);
      const key = JSON.stringify([source, listing.id]);
      if (!imageURL || seen.has(key)) continue;
      seen.add(key);
      products.push({ id: listing.id, title: listing.title, imageURL, source });
    }
  };
  if (etsyData && etsyData.source === 'etsy') addProducts(etsyData.listings, 'etsy');
  if (siteData) addProducts(siteData.products, 'site');

  for (let i = products.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [products[i], products[j]] = [products[j], products[i]];
  }
  if (!products.length) return Promise.resolve();

  const anchors = [...document.querySelectorAll('a[data-product-image]')]
    .filter((anchor) => anchor.querySelector('img'));
  return Promise.all(anchors.map((anchor, index) => {
    const product = products[index % products.length];
    const image = anchor.querySelector('img');
    const original = ['src', 'srcset', 'alt'].map((name) => image.getAttribute(name));
    return new Promise((resolve) => {
      const preload = new Image();
      preload.onload = () => {
        image.addEventListener('error', () => {
          ['src', 'srcset', 'alt'].forEach((name, i) => {
            if (original[i] === null) image.removeAttribute(name);
            else image.setAttribute(name, original[i]);
          });
          anchor.removeAttribute('href');
        }, { once: true });
        image.removeAttribute('srcset');
        image.setAttribute('alt', product.title);
        image.setAttribute('src', product.imageURL);
        anchor.href = '/product?' + new URLSearchParams({ source: product.source, id: product.id }).toString();
        resolve();
      };
      preload.onerror = () => resolve();
      preload.src = product.imageURL;
    });
  }));
}

const editorialImagesReady = Promise.all([
  setupCarousel(document.querySelector('#etsy-featured'), document.querySelector('#featured-track'), document.querySelector('#featured-status'), '/api/featured-listings', 'etsy'),
  setupCarousel(document.querySelector('#site-featured'), document.querySelector('#site-track'), document.querySelector('#site-status'), '/api/products', 'site')
]).then(([etsyData, siteData]) => setupEditorialImages(etsyData, siteData));
