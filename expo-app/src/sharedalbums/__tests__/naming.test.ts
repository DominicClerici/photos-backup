import { sharedResourceName } from '../naming';

describe('sharedResourceName', () => {
  // The case that put thirty-nine JPEGs into the archive labelled image/heic:
  // Apple re-encodes what goes into a shared album and keeps the old name.
  it('renames a shared still that Apple re-encoded to JPEG', () => {
    expect(sharedResourceName('IMG_6822.HEIC', 'public.jpeg')).toBe('IMG_6822.jpg');
  });

  it('keeps the stem, which is how the photograph is known', () => {
    expect(sharedResourceName('holiday-2019 (1).HEIC', 'public.jpeg')).toBe('holiday-2019 (1).jpg');
  });

  it('leaves a name that already matches the bytes alone', () => {
    expect(sharedResourceName('IMG_1.jpg', 'public.jpeg')).toBe('IMG_1.jpg');
    expect(sharedResourceName('IMG_2.HEIC', 'public.heic')).toBe('IMG_2.HEIC');
  });

  // Renaming .jpeg to .jpg would churn a filename in the archive to say nothing
  // that was not already true.
  it('leaves an extension that is only spelled differently', () => {
    expect(sharedResourceName('IMG_3.jpeg', 'public.jpeg')).toBe('IMG_3.jpeg');
    expect(sharedResourceName('CLIP.m4v', 'public.mpeg-4')).toBe('CLIP.m4v');
  });

  it('names a paired video after the movie iCloud actually sent', () => {
    expect(sharedResourceName('IMG_6822.MOV', 'com.apple.quicktime-movie')).toBe('IMG_6822.MOV');
    expect(sharedResourceName('IMG_6822.MOV', 'public.mpeg-4')).toBe('IMG_6822.mp4');
  });

  it('gives an extension to a name that has none', () => {
    expect(sharedResourceName('IMG_7266', 'com.apple.quicktime-movie')).toBe('IMG_7266.mov');
  });

  // An identifier this does not know is not evidence about anything, and a name
  // overwritten on that basis would be worse than one that is merely wrong.
  it('leaves the name alone for a type it cannot place', () => {
    expect(sharedResourceName('IMG_9.HEIC', 'com.apple.something-new')).toBe('IMG_9.HEIC');
    expect(sharedResourceName('IMG_9.HEIC', null)).toBe('IMG_9.HEIC');
  });
});
